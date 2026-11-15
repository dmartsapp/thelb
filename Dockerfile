# syntax=docker/dockerfile:1

# theLB's container image: the release's own binary, nothing built here. The
# release workflow builds, attests and checksums the binaries first, then
# copies each into the image for its platform - the image runs the very file
# the release publishes.
#
# distroless/static: CA certificates and time zones, no shell, no package
# manager. It runs as root - theLB requires it, and a director's pod is
# privileged on the host's network anyway (see `thelb generate k8s director`).
FROM gcr.io/distroless/static-debian12

ARG VERSION
ARG TARGETARCH

COPY dist/thelb-${VERSION}-linux-${TARGETARCH} /usr/local/bin/thelb

ENTRYPOINT ["/usr/local/bin/thelb"]
CMD ["--help"]
