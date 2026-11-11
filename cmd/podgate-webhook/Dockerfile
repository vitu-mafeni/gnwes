FROM gcr.io/distroless/static:nonroot
COPY bin/podgate-webhook /podgate-webhook
COPY bin/webhook-certs/tls.crt /certs/tls.crt
COPY bin/webhook-certs/tls.key /certs/tls.key
ENTRYPOINT ["/podgate-webhook"]
