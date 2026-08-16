# Contributing to Vyshka

Thanks for your interest. Vyshka is in the **design phase**: the deliverable right now is
the protocol spec and its conformance suites, not code. Contributions that matter most at
this stage are design review, protocol feedback, and firsthand knowledge of engine
constraints (DayZ `RestApi` behavior, Arma Reforger scripting capabilities).

## How the project works

- **Spec-first.** The protocol document is the single source of truth. Design changes go
  into the spec, not into side notes or code comments. Substantive edits bump the draft
  version and date in the spec header.
- **Normative language.** The spec follows RFC 2119; use MUST/SHOULD/MAY deliberately.
- **Forward compatibility is load-bearing.** Unknown envelope `type` values are acked and
  ignored, never fatal. The envelope version `v` bumps only on envelope-breaking changes;
  body-level evolution is additive. Any protocol edit must preserve this.

## Clean-room provenance policy (binding)

Vyshka is original work. Commercial products in this space were evaluated **only** to
establish what capabilities server admins expect and where those products fall short. The
requirements came from that evaluation; the design and code must not. Hard rules for every
contribution:

1. **No copied or ported code** from any third-party product's repositories. In particular,
   unlicensed source (no license file means all rights reserved) must not be open in front
   of you while you write plugin code. If you have studied such source in depth, contribute
   to the spec and conformance tests instead of the corresponding plugin (standard
   clean-room separation).
2. **No lookalike naming.** No identifiers, action codes, config keys, or class names that
   imitate any existing product's naming. All names originate here.
3. **No protocol imitation.** Wire protocol, endpoints, and message shapes are designed
   from requirements, never transcribed from another product's observed traffic or source.
4. **No third-party trademarks** in product naming, packaging, or documentation.

If a contribution cannot clearly satisfy these rules, it will be declined regardless of
technical quality.

## Practical notes

- Open an issue to discuss protocol changes before writing them up; envelope-level changes
  in particular need consensus.
- Keep PRs focused: one design concern per PR.
- Once code exists, plugin and hub changes must keep their conformance suites green.

## License

By contributing, you agree that your contributions are licensed under the
[Apache License 2.0](LICENSE).
