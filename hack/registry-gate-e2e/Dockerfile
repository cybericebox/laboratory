# The image the registry gate E2E runs: the node binary (the gate is `/node registry-gate`) and the relay of a node, on a base with a shell
# (the registry pod's own-data init container runs /bin/sh).
FROM alpine:3.22
COPY node /node
COPY relay /relay
