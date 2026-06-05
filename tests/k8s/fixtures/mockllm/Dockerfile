FROM golang:1.26-alpine AS builder
WORKDIR /src
COPY go.mod ./
COPY main.go ./
COPY datasheet/ ./datasheet/
RUN CGO_ENABLED=0 go build -ldflags="-s -w" -o /out/mockllm ./

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=builder /out/mockllm /mockllm
EXPOSE 8000
USER nonroot:nonroot
ENTRYPOINT ["/mockllm"]
