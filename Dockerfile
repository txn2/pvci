FROM alpine:3.12.0 AS util

RUN echo "nobody:x:65534:65534:Nobody:/:" > /etc_passwd
RUN apk update && apk --no-cache add ca-certificates

FROM scratch

ENV PATH=/bin

COPY pvci /bin/
COPY --from=util /etc_passwd /etc/passwd
COPY --from=alpine /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/

WORKDIR /

USER nobody
ENTRYPOINT ["/bin/pvci"]