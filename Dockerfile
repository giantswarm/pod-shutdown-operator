# Use distroless as minimal base image to package the pod-shutdown-operator binary
# Refer to https://github.com/GoogleContainerTools/distroless for more details
FROM gcr.io/distroless/static:nonroot
ARG TARGETARCH
WORKDIR /

COPY pod-shutdown-operator-linux-${TARGETARCH} /pod-shutdown-operator

USER 65532:65532

ENTRYPOINT ["/pod-shutdown-operator"]
