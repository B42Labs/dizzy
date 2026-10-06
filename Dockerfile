FROM alpine:3.24.2@sha256:294b683cb724975bec92580e1e685676bd4b50bda910ddb8c51d4cabeaec77e6

ARG TARGETARCH

LABEL org.opencontainers.image.source="https://github.com/B42Labs/dizzy" \
      org.opencontainers.image.title="dizzy" \
      org.opencontainers.image.description="A scenario-driven load and consistency tester for OpenStack control planes"

COPY --chmod=0755 bin/dizzy-linux-${TARGETARCH} /usr/local/bin/dizzy
# .dockerignore admits only scenarios/*/*.yaml, so scenarios.go stays out.
COPY scenarios/ /usr/share/dizzy/scenarios/

# USER precedes WORKDIR so BuildKit creates /work owned by 65534:65534.
USER 65534:65534
WORKDIR /work
ENTRYPOINT ["/usr/local/bin/dizzy"]
