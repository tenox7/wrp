FROM golang AS builder
WORKDIR /src
RUN git clone https://github.com/tenox7/wrp.git
WORKDIR /src/wrp
RUN go mod download
ARG TARGETARCH
RUN CGO_ENABLED=0 GOOS=linux GOARCH=${TARGETARCH} go build -o /wrp-${TARGETARCH}

FROM chromedp/headless-shell
ARG TARGETARCH
COPY --from=builder /wrp-${TARGETARCH} /wrp
ENTRYPOINT ["/wrp"]
ENV PATH="/headless-shell:${PATH}"
LABEL maintainer="as@tenoware.com"
