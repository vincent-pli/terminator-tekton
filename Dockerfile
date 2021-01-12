FROM registry.redhat.io/ubi8/ubi-minimal

WORKDIR /

COPY _output/bin/terminator /usr/local/bin

ENTRYPOINT []
CMD ["terminator"]
