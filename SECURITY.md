# Security Policy

## Supported versions

The hub and the DayZ plugin are released independently (`RELEASING.md`); the first tags
were hub 0.1.0 and DayZ plugin 0.7.0 (2026-09-17), and the latest are hub 0.2.0 and DayZ
plugin 0.8.0 (2026-09-23). Before 1.0, only the latest release of
each artifact receives security fixes, as a new patch or minor release; there are no
backports to earlier 0.x versions. The protocol document is a draft and fixes to it land
in the next draft.

## Reporting a vulnerability

Security issues in the protocol design are just as important as implementation bugs:
authentication realms, token scoping, webhook signing, and delivery guarantees are all in
scope.

- **Do not** open a public issue for anything exploitable.
- Report privately via GitHub Security Advisories on this repository
  ("Report a vulnerability"), or by email to <americanemt@gmail.com>.
- Include what you found, where (spec section or file), and impact as you understand it.

You can expect an acknowledgment within 7 days. Please allow a reasonable window for a fix
before public disclosure; we will credit reporters in the advisory unless you prefer
otherwise.
