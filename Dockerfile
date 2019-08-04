FROM chromedp/headless-shell
ADD wrp /wrp
ENTRYPOINT ["/wrp"]
ENV PATH="/headless-shell:${PATH}"
LABEL maintainer="as@tenoware.com"
