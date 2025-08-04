# WRP - Web Rendering Proxy

A browser-in-browser "proxy" server that allows to use historical / vintage web browsers on the modern web. It has two modes:

- ISMAP "graphical" mode, renders web page in to a GIF, PNG or JPG image with clickable imagemap.
- Simple HTML "text" mode converts web page in to Markdown, then renders it into simplified HTML for old browsers.

![Internet Explorer 1.5 doing Gmail](wrp.png)

## Usage Instructions

* [Download a WRP binary](https://github.com/tenox7/wrp/releases/) run it on a machine that will become your WRP gateway/server. This should be modern hardware and OS. Google Chrome / Chromium Browser is required to be preinstalled. Do not try to run WRP on an old machine like Windows XP or 98.
* Make sure you have disabled firewall or open port WRP is listening on (by default 8080).
* Point your legacy browser to `http://address:port` of the WRP server. Do not set or use it as a "proxy server".
* Type a search string or a full http/https URL and click **Go**.
* Select whether you want to use graphical (ISMAP) or simple HTML mode.

### Image Map Mode

* Adjust your screen **W**idth/**H**eight/**S**cale/**C**olors to fit in your old browser.
* Scroll web page by clicking on the in-image scroll bar on the right.
* WRP also allows **a single tall image without the vertical scrollbar** and use client scrolling. To enable this, simply height **H** to `0` (or flag `-g 1152x0x216`. However this should not be used with old and low spec clients. Such tall images will be very large, take a lot of memory and long time to process, especially for GIFs.
* Do not use client browser history-back, instead use **Bk** button in the app.
* You can re-capture page screenshot without reloading by using **St** (Stop). This is useful if page didn't render fully before screenshot is taken.
* You can also reload and re-capture current page with **Re** (Reload).
* To send keystrokes, fill **K** input box and press **Go**. There also are buttons for backspace, enter and arrow keys.
* The default image type GIP is a ultra fast, optimized, parallel encoded GIF type.
* If your browser supports it, prefer PNG over GIF/JPG. PNG is much faster, whereas GIF/JPG requires a lot of additional processing on both client and server to encode/decode.
* GIF images are by default encoded with 216 colors, "web safe" palette. This uses an ultra fast but not very accurate color mapping algorithm. If you want better color representation switch to 256 color mode.

### Simple HTML mode

* Select image type PNG/GIF/JPG. Each individual image from the original web site will be converted to the selected format.
* Type maximum image size in pixels.

## UI explanation

The first unnamed input box is either search (google) or URL starting with http/https

`Go` Navigate to the url or perform search

`Bk` History Back

`St` Stop, also re-capture screenshot without refreshing page, for example if page
render takes a long time or it updates / changes periodically

`Re` Remote Reload / Refresh

`Up` Page Up

`Dn` Page Down

`W` is width in pixels, adjust it to get rid of horizontal scroll bar

`H` is height in pixels, adjust it to get rid of vertical scroll bar.
It can also be set to 0 to produce one very tall image and use
client scroll. This 0 size is experimental, buggy and should be
used with PNG and lots of memory on a client side.

`Z` Zoom or scale

`M` Mode - ISMAP (clickable imagemap) or simple HTML mode

`T` Image type PNG / GIF / JPEG

`C` Colors, for GIF images only

`K` Keystroke input, you can type some letters in it and when you click Go it will be typed in the remote browser.

`Bs` Backspace

`Rt` Return / enter

### UI Customization

WRP supports customizing it's own UI using HTML Template file. Download [wrp.html](wrp.html) place in the same directory with wrp binary customize it to your liking.

## Docker

https://hub.docker.com/r/tenox7/wrp

```shell
$ docker run -d --rm -p 8080:8080 tenox7/wrp:latest
```

## AWS

It's possible to run WRP on AWS App Runner.

First you need to upload the Docker image to ECR - [Instructions](https://docs.aws.amazon.com/AmazonECR/latest/userguide/docker-push-ecr-image.html).

Create App Runner service using the uploaded image using the AWS Console or CLI.

[AWS Console](https://console.aws.amazon.com/apprunner/home#/create)

```shell
aws apprunner create-service --service-name my-app-runner-service --source-configuration '{
    "ImageRepository": {
        "ImageIdentifier": "<account_id>.dkr.ecr.<region>.amazonaws.com/wrp:latest",
        "ImageRepositoryType": "ECR",
        "ImageConfiguration": {"Port": "8000"},
        "AutoDeploymentsEnabled": true
    }
}' --instance-configuration '{
    "Cpu": "1024",
    "Memory": "2048",
    "InstanceRoleArn": "arn:aws:iam::<account_id>:role/AppRunnerECRAccessRole"
}'
```

## Azure Container Instances

[Azure Console](https://portal.azure.com/#create/Microsoft.ContainerInstances)

CLI:

```shell
$ az container create --resource-group wrp --name wrp --image tenox7/wrp:latest --cpu 1 --memory 2 --ports 80 --protocol tcp --os-type Linux --ip-address Public --command-line '/wrp -l :80 -t png -g 1280x0x256'
```

## Google Cloud Run

```shell
$ gcloud run deploy --platform managed --image=tenox7/wrp:latest --memory=2Gi --args='-t=png','-g=1280x0x256'
```

Unfortunately Google Cloud Run forces you to use HTTPS, which likely won't work with old browsers.


## Flags

```text
-l   listen address:port (default :8080)
-m   mode, either ismap (graphical) or html
-t   image type gif, png or jpg (default gif)
-g   image geometry, WxHxC, height can be 0 for unlimited (default 1152x600x216)
     C (number of colors) is only used for GIF
-q   Jpeg image quality, default 75%
-h   headless mode, hide browser window on the server (default true)
-n   do not free maps and images after use (default false)
-ui  html template file (default "wrp.html")
-ua  user agent, override the default "headless" agent (only for ismap mode)
-s   delay/sleep after page is rendered before screenshot is taken (default 2s)
-b   browser executable path (e.g., for Brave Browser)
```

## Minimal Requirements

* Server/Gateway requires modern hardware and operating system that is supported by [Go language](https://github.com/golang/go/wiki/MinimumRequirements) and Chrome/Chromium Browser, which must be installed.
* Client Browser needs to support `HTML FORMs` and `ISMAP`. Typically [Mosaic 2.0](http://www.ncsa.illinois.edu/enabling/mosaic/versions) would be minimum version for forms. However ISMAP was supported since 0.6B, so if you manually enter url using `?url=...`, you can use the earlier version.

## FAQ

### I can't get it to run

This program does not have a GUI and is run from the command line. After downloading, you may need to enable executable bit on Unix systems, for example:

```shell
$ cd ~/Downloads
$ chmod +x wrp-amd64-macos
$ ./wrp-amd64-macos
```

### Websites are blocking headless browsers

This is a well known issue. WRP has some provisions to work around it, but it's a cat and mouse game. By default WRP tries to obtain some current valid User Agent
from https://github.com/jnrbsn/user-agents rather than using the internal "HeadlessChrome". You can override this to your own, for example:

```shell
$ wrp -ua="Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/119.0.0.0 Safari/537.36"
```

### Why is WRP called "proxy" when it's not

WRP originally started as true http proxy. However this stopped working because the whole internet is now encrypted thanks to [Let's Encrypt](https://en.wikipedia.org/wiki/Let%27s_Encrypt). Legacy browsers do not support modern SSL/TLS certs as well as [HTTP CONNECT](https://en.wikipedia.org/wiki/HTTP_tunnel#HTTP_CONNECT_method) so this mode had to be disabled.

### Will you support http proxy mode in future?

Some efforts (ssl strip) are under way but it's very [difficult](https://en.wikipedia.org/wiki/HTTP_tunnel#HTTP_CONNECT_method) to do it correctly and the priority is rather low.

### Why isn't there a Docker image for armv6

Because https://hub.docker.com/r/chromedp/headless-shell/ doesn't have one. WRP uses that image. If you have a fork that builds for armv6 let me know.

### WTF is GIP image format

It's just GIF but optimized. Avoids dithering, uses fast color palette  and parallel encoding. https://github.com/tenox7/gip

## History

* Version 1.0 (2014) started as a *cgi-bin* script, adaptation of `webkit2png.py` and `pcidade.py`, [blog post](https://virtuallyfun.com/2014/03/03/surfing-modern-web-with-ancient-browsers/).
* Version 2.0 became a stand alone http-proxy server, supporting both Linux and MacOS, [another post](https://virtuallyfun.com/wordpress/2014/03/11/web-rendering-proxy-update//).
* In 2016 thanks to [Let's Encrypt](https://en.wikipedia.org/wiki/Let%27s_Encrypt) the whole internet migrated to HTTPS/SSL/TLS and WRP largely stopped working. Python code became unmaintainable and there was no easy way to make it work on Windows, even under WSL.
* Version 3.0 (2019) has been rewritten in [Go](https://golang.org/) using [Chromedp](https://github.com/chromedp) as browser-in-browser instead of http-proxy. The initial version was [less than 100 lines of code](https://gist.github.com/tenox7/b0f03c039b0a8b67f6c1bf47e2dd0df0).
* Version 4.0 has been completely refactored to use mouse clicks via imagemap instead parsing a href nodes.
* Version 4.1 added sending keystrokes in to input boxes. You can now login to Gmail. Also now runs as a Docker container and on Cloud Run/Azure Containers.
* Version 4.5 introduces rendering whole pages in to a single tall image with client scrolling.
* Version 4.6 adds blazing fast gif encoding by [Hill Ma](https://github.com/mahiuchun).
* Version 4.6.3 adds arm64 / aarch64 Docker container support - you can run it on Raspberry PI!
* Version 4.7 add simple html aka reader aka text mode.
* Version 4.8 add image support to simple html mode.
* Version 4.9 adds support for ultra fast, parallel encoded gif image (GIP)

## Credits

* Uses [chromedp](https://github.com/chromedp), thanks to [mvdan](https://github.com/mvdan) for dealing with my issues
* Uses [go-quantize](https://github.com/ericpauley/go-quantize), thanks to [ericpauley](https://github.com/ericpauley) for developing the missing go quantizer
* Thanks to Jason Stevens of [Fun With Virtualization](https://virtuallyfun.com/) for graciously hosting my rumblings
* Thanks to [claunia](https://github.com/claunia/) for help with the Python/Webkit version in the past
* Thanks to [Hill Ma](https://github.com/mahiuchun) for ultra fast gif encoding algorithm
* Historical Python/Webkit versions and prior art can be seen in [wrp-old](https://github.com/tenox7/wrp-old) repo

## Related

You may also be interested in:

* [VncFox](https://github.com/tenox7/vncfox)
* [Browservice](https://github.com/ttalvitie/browservice)
* [Browsh](https://github.com/browsh-org/browsh)

## Legal Stuff

```text
License: Apache 2.0
Copyright (c) 2013-2025 Antoni Sawicki
```
