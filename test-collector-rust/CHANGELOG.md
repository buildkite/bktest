# Changelog

## 0.1.5

### Security

- Require `rustls` 0.23.46 or later, including fresh dependency resolutions,
  to address [RUSTSEC-2026-0285](https://rustsec.org/advisories/RUSTSEC-2026-0285.html),
  [RUSTSEC-2026-0346](https://rustsec.org/advisories/RUSTSEC-2026-0346.html),
  [RUSTSEC-2026-0348](https://rustsec.org/advisories/RUSTSEC-2026-0348.html) and
  [RUSTSEC-2026-0349](https://rustsec.org/advisories/RUSTSEC-2026-0349.html).
- Clarify that the eMudhra fixes in 0.1.4 address both RUSTSEC-2026-0381
  (`webpki-roots`) and RUSTSEC-2026-0382 (`rustls-webpki`). Existing users
  should rebuild with `cargo install --force buildkite-test-collector`.

## 0.1.4

### Security

- Update the bundled TLS trust store and certificate verifier for the eMudhra
  intermediate CA key compromise
  ([CVE-2026-111943 / RUSTSEC-2026-0381 / RUSTSEC-2026-0382](https://emudhra.we-have-a-problem.com/)).
  Require `webpki-roots` 1.0.10 or later and `rustls-webpki` 0.103.16 or later,
  including when installing without the committed lockfile. Existing users
  should rebuild with `cargo install --force buildkite-test-collector`.
