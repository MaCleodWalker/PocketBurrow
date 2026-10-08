FROM alpine:3.22 AS download
ARG PB_VERSION=0.40.5
ARG TARGETARCH=amd64
RUN apk add --no-cache ca-certificates curl unzip
RUN curl --fail --location --output /tmp/pb.zip \
    "https://github.com/pocketbase/pocketbase/releases/download/v${PB_VERSION}/pocketbase_${PB_VERSION}_linux_${TARGETARCH}.zip" \
    && unzip /tmp/pb.zip pocketbase -d /pb

FROM alpine:3.22
RUN apk add --no-cache ca-certificates \
    && addgroup -g 10001 pb && adduser -D -u 10001 -G pb pb \
    && mkdir -p /pb/pb_data && chown -R pb:pb /pb
COPY --from=download /pb/pocketbase /pb/pocketbase
USER 10001:10001
WORKDIR /pb
EXPOSE 8090
CMD ["/pb/pocketbase", "serve", "--http=0.0.0.0:8090", "--dir=/pb/pb_data"]
