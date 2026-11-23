# The kumaco image. Its build context holds the static binary of each
# platform, as <os>/<arch>/kumaco: GoReleaser builds them for a release, and
# `mise run image` for local use (see AGENTS.md, "Releases and images").
#
# FROM scratch: the binary needs nothing from a base image. It embeds the time
# zone database (README.md, "Time zones"), and reaches the API server with the
# CA of its service account.
FROM scratch
ARG TARGETPLATFORM
COPY ${TARGETPLATFORM}/kumaco /kumaco
# A numeric non-root user: there is no /etc/passwd to resolve a name.
USER 65532:65532
ENTRYPOINT ["/kumaco"]
