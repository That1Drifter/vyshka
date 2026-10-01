// Grades answers from a live hub against spec/openapi-admin.yaml: each one
// must use a status the operation declares and satisfy that status's schema,
// and an object may carry no member its schema does not declare (unless the
// schema admits additional properties). The last rule is the one a plain
// schema validator cannot apply, since the document leaves most objects
// open, and it is what catches a hub that grew a field the document never
// learned about.

import { readFile } from "node:fs/promises";
import { Ajv2020, type ValidateFunction } from "ajv/dist/2020.js";
import addFormatsModule from "ajv-formats";
import { parse } from "yaml";

// ajv-formats is CommonJS; under Node's ESM loader its default export is
// the module object.
const addFormats = ((addFormatsModule as unknown as { default?: unknown }).default ??
  addFormatsModule) as unknown as (ajv: Ajv2020) => void;

type Schema = Record<string, unknown>;

const methods = ["get", "put", "post", "delete", "patch"] as const;

export interface Operation {
  id: string;
  method: string;
  path: string;
}

export class OpenAPICheck {
  readonly operations: Operation[] = [];
  readonly failures: string[] = [];
  readonly exercised = new Set<string>();
  private readonly doc: Schema;
  private readonly ajv: Ajv2020;
  private readonly validators = new Map<string, ValidateFunction>();

  private constructor(doc: Schema) {
    this.doc = doc;
    this.ajv = new Ajv2020({ strict: false, allErrors: true, validateSchema: false });
    addFormats(this.ajv);
    this.ajv.addSchema({ ...doc, $id: "openapi" });
    const paths = (doc.paths ?? {}) as Record<string, Schema>;
    for (const [path, item] of Object.entries(paths)) {
      for (const method of methods) {
        const operation = item[method] as Schema | undefined;
        if (operation) {
          this.operations.push({ id: String(operation.operationId), method: method.toUpperCase(), path });
        }
      }
    }
  }

  static async load(url: URL): Promise<OpenAPICheck> {
    return new OpenAPICheck(parse(await readFile(url, "utf8")) as Schema);
  }

  /** Grades one answer; a fault is recorded, not thrown. */
  async grade(method: string, schemaPath: string, response: Response): Promise<void> {
    const where = `${method} ${schemaPath} -> ${response.status}`;
    const item = (this.doc.paths as Record<string, Schema>)[schemaPath];
    const operation = item?.[method.toLowerCase()] as Schema | undefined;
    if (!operation) {
      this.failures.push(`${where}: the document has no such operation`);
      return;
    }
    const responses = operation.responses as Record<string, Schema>;
    let declared = responses[String(response.status)];
    if (!declared) {
      this.failures.push(`${where}: ${operation.operationId} declares no ${response.status} answer`);
      return;
    }
    // Where the answer is declared, as a JSON pointer: in the operation, or
    // in components/responses when the operation refers to one there.
    let pointer = refPointer(["paths", schemaPath, method.toLowerCase(), "responses", String(response.status)]);
    if (typeof declared.$ref === "string") {
      pointer = declared.$ref.replace(/^#/, "");
      declared = this.resolve(declared.$ref);
    }
    const text = await response.text();
    const content = declared.content as Record<string, Schema> | undefined;
    const media = content?.["application/json"];
    if (!media) {
      if (text.trim() !== "") {
        this.failures.push(`${where}: the document declares no body and the hub sent ${text.slice(0, 200)}`);
      }
    } else {
      let body: unknown;
      try {
        body = JSON.parse(text);
      } catch {
        this.failures.push(`${where}: the body is not JSON: ${text.slice(0, 200)}`);
        return;
      }
      const validate = this.validator(`${pointer}/content/application~1json/schema`);
      if (!validate(body)) {
        for (const error of validate.errors ?? []) {
          this.failures.push(`${where}: ${error.instancePath || "/"} ${error.message} ${JSON.stringify(error.params)}`);
        }
      }
      const unknown: string[] = [];
      this.undeclared(media.schema as Schema, body, "", unknown);
      for (const member of unknown) {
        this.failures.push(`${where}: the hub sent ${member}, which the document does not declare`);
      }
    }
    if (response.status >= 200 && response.status < 300) {
      this.exercised.add(String(operation.operationId));
    }
  }

  /** The operations no 2xx answer has graded yet. */
  unexercised(): string[] {
    return this.operations.filter((op) => !this.exercised.has(op.id)).map((op) => `${op.id} (${op.method} ${op.path})`);
  }

  private validator(pointer: string): ValidateFunction {
    let validate = this.validators.get(pointer);
    if (!validate) {
      // Validating through the document keeps every local $ref resolvable.
      validate = this.ajv.compile({ $ref: `openapi#${pointer}` });
      this.validators.set(pointer, validate);
    }
    return validate;
  }

  resolve(ref: string): Schema {
    let node: unknown = this.doc;
    for (const part of ref.replace(/^#\//, "").split("/")) {
      node = (node as Schema)[part.replaceAll("~1", "/").replaceAll("~0", "~")];
    }
    if (!node || typeof node !== "object") {
      throw new Error(`unresolved $ref ${ref}`);
    }
    return node as Schema;
  }

  /** Collects members of value that schema does not declare. */
  private undeclared(schema: Schema | undefined, value: unknown, path: string, out: string[]): void {
    if (!schema || value === null || typeof value !== "object") {
      return;
    }
    if (typeof schema.$ref === "string") {
      this.undeclared(this.resolve(schema.$ref), value, path, out);
      return;
    }
    if (Array.isArray(value)) {
      for (const [index, element] of value.entries()) {
        this.undeclared(schema.items as Schema | undefined, element, `${path}[${index}]`, out);
      }
      return;
    }
    // The object branches of a composition, flattened: allOf contributes every
    // branch, oneOf and anyOf the branches that are objects (the others are
    // the null of a nullable member).
    const branches = this.objectBranches(schema);
    if (branches.length === 0) {
      return;
    }
    if (branches.some((branch) => branch.additionalProperties !== undefined && branch.additionalProperties !== false)) {
      // An open object: what it declares is still graded, the rest is not.
      for (const branch of branches) {
        for (const [name, member] of Object.entries((branch.properties ?? {}) as Record<string, Schema>)) {
          if (name in (value as object)) {
            this.undeclared(member, (value as Record<string, unknown>)[name], `${path}.${name}`, out);
          }
        }
      }
      return;
    }
    if (!branches.some((branch) => branch.properties !== undefined)) {
      return;
    }
    for (const [name, member] of Object.entries(value as Record<string, unknown>)) {
      const declaring = branches.find((branch) => name in ((branch.properties ?? {}) as Schema));
      if (!declaring) {
        out.push(`${path}.${name}`);
        continue;
      }
      this.undeclared((declaring.properties as Record<string, Schema>)[name], member, `${path}.${name}`, out);
    }
  }

  private objectBranches(schema: Schema): Schema[] {
    if (typeof schema.$ref === "string") {
      return this.objectBranches(this.resolve(schema.$ref));
    }
    const out: Schema[] = [];
    if (schema.properties !== undefined || schema.additionalProperties !== undefined || schema.type === "object") {
      out.push(schema);
    }
    for (const branch of (schema.allOf ?? []) as Schema[]) {
      out.push(...this.objectBranches(branch));
    }
    for (const branch of [...((schema.oneOf ?? []) as Schema[]), ...((schema.anyOf ?? []) as Schema[])]) {
      out.push(...this.objectBranches(branch));
    }
    return out;
  }
}

function refPointer(parts: string[]): string {
  return "/" + parts.map((part) => part.replaceAll("~", "~0").replaceAll("/", "~1")).join("/");
}
