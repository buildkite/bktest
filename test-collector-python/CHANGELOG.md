# Changelog

## 1.9.1 — 2026-10-20

- Require certifi >= 2026.10.19 as defence in depth against the eMudhra
  intermediate CA key compromise ([CVE-2026-111943](https://emudhra.we-have-a-problem.com/)).
  Existing releases are not yanked; users should upgrade to receive the new
  minimum trust-store version.
- Update the locked trust store and pin CI to uv 0.13.4. Refresh system CA
  certificates and require patched pip and certifi for package builds and
  publishing, including twine's isolated environment.
