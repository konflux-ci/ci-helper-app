FROM registry.access.redhat.com/ubi9/go-toolset:1.20.10 AS builder

COPY . .

RUN make build

FROM registry.access.redhat.com/ubi9/ubi-minimal:9.3
COPY --from=builder /opt/app-root/src/ci-helper-app /
COPY --from=builder /opt/app-root/src/config.yaml /
USER 65532:65532

ENTRYPOINT ["/ci-helper-app"]