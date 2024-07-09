FROM chromedp/headless-shell
ARG TARGETARCH
ADD wrp-${TARGETARCH}-linux /wrp
ENTRYPOINT ["/wrp"]
ENV PATH="/headless-shell:${PATH}"
LABEL maintainer="as@tenoware.com"
