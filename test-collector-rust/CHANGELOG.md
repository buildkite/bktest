# Changelog

## 0.1.4

### Security

- Update the bundled TLS trust store and certificate verifier for the eMudhra
  intermediate CA key compromise
  ([CVE-2026-111943 / RUSTSEC-2026-0381](https://emudhra.we-have-a-problem.com/)).
  Require `webpki-roots` 1.0.10 or later and `rustls-webpki` 0.103.16 or later,
  including when installing without the committed lockfile. Existing users
  should rebuild with `cargo install --force buildkite-test-collector`.
