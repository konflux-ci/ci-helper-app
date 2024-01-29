FROM registry.access.redhat.com/ubi9/go-toolset:1.20.10 AS builder

COPY go.mod go.mod
COPY go.sum go.sum

RUN go mod download

COPY example/ example/
COPY githubapp/ githubapp/
COPY Makefile Makefile

RUN make build

FROM registry.access.redhat.com/ubi9/ubi-minimal:9.3
COPY --from=builder /opt/app-root/src/ci-helper-app /
COPY --from=builder /opt/app-root/src/example/config.yml /example/config.yml
USER 65532:65532

ENTRYPOINT ["/ci-helper-app"]