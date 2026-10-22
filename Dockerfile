FROM alpine:3.24.3@sha256:b3413572228e34bd1c9245f5565e6cd127005655e8a7899aef33d8e83255c9e6

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
