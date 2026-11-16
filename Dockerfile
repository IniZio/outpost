# The server image: `outpost` only. The machine agent runs on the machines, not here.
FROM golang:1.26.11 AS build
ENV GOTOOLCHAIN=local CGO_ENABLED=0
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
ARG VERSION=dev
ARG COMMIT=unknown
RUN go build -trimpath -ldflags "-s -w" -o /out/outpost ./cmd/outpost

FROM gcr.io/distroless/static-debian12:nonroot
ARG COMMIT=unknown
LABEL org.opencontainers.image.source=https://github.com/IniZio/outpost org.opencontainers.image.revision=$COMMIT
COPY --from=build /out/outpost /outpost
USER 65532:65532
EXPOSE 7400
ENTRYPOINT ["/outpost"]
CMD ["serve"]
