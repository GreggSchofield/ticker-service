FROM golang:1.25-alpine AS builder

WORKDIR /app

COPY go.mod go.sum* ./

RUN go mod download

COPY *.go ./

RUN CGO_ENABLED=0 GOOS=linux go build -o stock-service .


FROM gcr.io/distroless/static-debian12:nonroot

COPY --from=builder /app/stock-service /

EXPOSE 8080

USER nonroot:nonroot

ENTRYPOINT ["/stock-service"]
