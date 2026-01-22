FROM golang:1.23.2 AS build

WORKDIR /src

COPY go.mod go.sum ./
RUN go mod download

COPY . .

ARG TARGETOS=linux
ARG TARGETARCH=amd64

RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH go build -o /out/pulse .

FROM gcr.io/distroless/base-debian12:nonroot

COPY --from=build /out/pulse /pulse

USER nonroot

EXPOSE 8080

ENTRYPOINT ["/pulse"]
