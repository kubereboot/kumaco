# Security Policy

## Supported Versions

kumaco releases versions in accordance with [the Semantic Version
specification](https://semver.org/):

- `<major>.<minor>.<patch>`
- Only the latest release is supported. Security fixes (e.g. CVEs fixed in
  dependencies) are not backported to older releases.
- Each release supports the Kubernetes versions listed in the
  [README's compatibility section](README.md#compatibility).
- Before `v1.0.0`, a minor release may contain breaking changes; see the
  [README's compatibility section](README.md#compatibility).
- Patch releases are released on an ongoing basis, whenever the team decides
  it's worth releasing. No guarantees.

The above describes the current development model. It's what our small team is
capable of delivering right now in terms of release management, testing, etc.

This means that the best support and most secure experience is always going to be
on the latest version of kumaco.

## Reporting a Vulnerability

kumaco is part of the Kured project. Please reach out to our private mailing list
`cncf-kured-maintainers@lists.cncf.io`. We will get back to you as soon as we can
and discuss the next steps to resolve the issue and disclose the issue
responsibly.

Thanks in advance for helping us make kumaco more secure.
