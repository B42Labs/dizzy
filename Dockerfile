FROM alpine:3.24.3@sha256:be2656061b536c1883c4034790e21d0a09c5aaaf51968b89368a3c0371ea2c9a

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
