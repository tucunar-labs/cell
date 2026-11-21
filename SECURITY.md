# Security reporting

Report suspected vulnerabilities privately to hola@tucunar.com. Include
the affected version, a minimal reproduction and expected impact, with
credentials and customer data removed. If that mailbox cannot be reached,
use the contact published at https://tucunar.com and request a private channel.

The current alpha receives security fixes on the main development branch.
Support commitments for a specific deployment are agreed separately.

Bind local forwards to loopback, protect operator and intake credentials,
restrict the cell namespace with NetworkPolicy, and keep cloud reporting
explicitly configured. Public read access is a development-only opt-in.

## Release signing key

Official releases of `tucunar-labs/cell` are signed with OpenSSH Ed25519
(`ssh-keygen -Y sign`, namespace `tucunar-cell-release`) using the public key
published in `allowed_signers`:

- Principal: `release@tucunar.com`
- Fingerprint: `SHA256:eQ180qR6xB4XD1Ko1crMX7pmuA9r6wheKbgYhkRp11s`

Always verify release checksums and signatures offline against this fingerprint
before importing OCI bundles or running scripts on a target host.
