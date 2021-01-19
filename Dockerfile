FROM registry.access.redhat.com/ubi8/ubi

WORKDIR /

COPY _output/bin/terminator /usr/local/bin

ENTRYPOINT []
CMD ["terminator"]
