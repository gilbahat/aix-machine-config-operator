# Operator image. The AIX agent is a separate binary (make agent-aix) that is
# installed on AIX nodes, not shipped in this image.
FROM docker.io/library/golang:1.26 AS build
WORKDIR /src
COPY . .
RUN make operator OUT=/out

FROM registry.access.redhat.com/ubi9/ubi-minimal:latest
COPY --from=build /out/linux/*/aix-machine-config-operator /usr/bin/aix-machine-config-operator
USER 65532:65532
ENTRYPOINT ["/usr/bin/aix-machine-config-operator"]
