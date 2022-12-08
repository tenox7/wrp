# WRP - Web Rendering Proxy

A browser-in-browser "proxy" server that allows to use historical / vintage web browsers on the modern web. It works by rendering a web page in to a GIF or PNG image with clickable imagemap.

![Internet Explorer 1.5 doing Gmail](wrp.png)

## Usage

* [Download a WRP binary](https://github.com/tenox7/wrp/releases/) and run it on a machine that will become your WRP gateway/server. This machine should be pretty modern, high spec and Google Chrome / Chromium Browser is required to be preinstalled.
* Make sure you don't have a firewall enabled or open up the port WRP is listening on (by default 8080).
* Point your legacy browser to `http://address:port` of the WRP server. Do not set or use it as a "proxy server".
* Type a search string or a full http/https URL and click **Go**.
* Adjust your screen **W**idth/**H**eight/**S**cale/**C**olors to fit in your old browser.
* Scroll web page by clicking on the in-image scroll bar.
* WRP also allows **a single tall image without the vertical scrollbar** and use client scrolling. To enable this, simply height **H** to `0` . However this should not be used with old and low spec clients. Such tall images will be very large, take a lot of memory and long time to process, especially for GIFs.
* Do not use client browser history-back, instead use **Bk** button in the app.
* You can re-capture page screenshot without reloading by using **St** (Stop). This is useful if page didn't render fully before screenshot is taken.
* You can also reload and re-capture current page with **Re** (Reload).
* To send keystrokes, fill **K** input box and press **Go**. There also are buttons for backspace, enter and arrow keys.
* Prefer PNG over GIF if your browser supports it. PNG is much faster, whereas GIF requires a lot of additional processing on both client and server to encode/decode. Jpeg encoding is also quite fast.
* GIF images are by default encoded with 216 colors, "web safe" palette. This uses an ultra fast but not very accurate color mapping algorithm. If you want better color representation switch to 256 color mode.

## Customization

WRP supports customizing it's own UI using HTML Template file. Download [wrp.html](wrp.html) place in the same directory with wrp binary customize it to your liking.

## Docker

```shell
$ docker run -d -p 80:8080 tenox7/wrp
```

## Google Cloud Run

```shell
$ gcloud run deploy --platform managed --image=gcr.io/tenox7/wrp:latest --memory=2Gi --args='-t=png','-g=1280x0x256'
```

Or from [Gcloud Console](https://console.cloud.google.com/run). Use `gcr.io/tenox7/wrp:latest` as container image URL.

Note that unfortunately GCR forces https. Your browser support of encryption protocols and certification authorities will vary. 

## Azure Container Instances

```shell
$ az container create --resource-group wrp --name wrp --image gcr.io/tenox7/wrp:latest --cpu 1 --memory 2 --ports 80 --protocol tcp --os-type Linux --ip-address Public --command-line '/wrp -l :80 -t png -g 1280x0x256'
```

Or from the [Azure Console](https://portal.azure.com/#create/Microsoft.ContainerInstances). Use `gcr.io/tenox7/wrp:latest` or `tenox7/wrp:latest` for image name.

Fortunately ACI allows port 80 without encryption.

## Flags

```text
-l   listen address:port (default :8080)
-t   image type gif, png or jpg (default gif)
-g   image geometry, WxHxC, height can be 0 for unlimited (default 1152x600x216)
     C (number of colors) is only used for GIF
-q   Jpeg image quality, default 80%
-h   headless mode, hide browser window on the server (default true)
-d   chromedp debug logging (default false)
-n   do not free maps and images after use (default false)
-ui  html template file (default "wrp.html")
-s   delay/sleep after page is rendered before screenshot is taken (default 2s)
```

## UI explanation

The first unnamed input box is either search (google) or URL starting with http/https

**Go** instructs browser to navigate to the url or perform search

**Bk** is History Back

**St** is Stop, also re-capture screenshot without refreshing page, for example if page
render takes a long time or it changes periodically

**Re** is Reload

**W** is width in pixels, adjust it to get rid of horizontal scroll bar

**H** is height in pixels, adjust it to get rid of vertical scroll bar.
It can also be set to 0 to produce one very tall image and use
client scroll. This 0 size is experimental, buggy and should be
used with PNG and lots of memory on a client side.

**Z** is zoom or scale

**C** is colors, for GIF images only (unused in PNG, JPG)

**K** is keystroke input, you can type some letters in it and when you click Go it will be typed in the remote browser.

**Bs** is backspace

**Rt** is return / enter

**< ^ v >** are arrow keys, typically for navigating a map, buggy.

## Minimal Requirements

* Server/Gateway requires modern hardware and operating system that is supported by [Go language](https://github.com/golang/go/wiki/MinimumRequirements) and Chrome/Chromium Browser, which must be installed.
* Client Browser needs to support `HTML FORMs` and `ISMAP`. Typically [Mosaic 2.0](http://www.ncsa.illinois.edu/enabling/mosaic/versions) would be minimum version for forms. However ISMAP was supported since 0.6B, so if you manually enter url using `?url=...`, you can use the earlier version.

## Troubleshooting

### I can't get it to run

This program does not have a GUI and is run from the command line. You may need to enable executable bit on Unix systems, for example:

```bash
$ cd ~/Downloads
$ chmod +x wrp-amd64-macos
$ ./wrp-amd64-macos -t png
```

## History

* Version 1.0 (2014) started as a *cgi-bin* script, adaptation of `webkit2png.py` and `pcidade.py`, [blog post](https://virtuallyfun.com/2014/03/03/surfing-modern-web-with-ancient-browsers/).
* Version 2.0 became a stand alone http-proxy server, supporting both Linux and MacOS, [another post](https://virtuallyfun.com/wordpress/2014/03/11/web-rendering-proxy-update//).
* In 2016 thanks to EFF/Certbot the whole internet migrated to HTTPS/SSL/TLS and WRP largely stopped working. Python code became unmaintainable and there was no easy way to make it work on Windows, even under WSL.
* Version 3.0 (2019) has been rewritten in [Go](https://golang.org/) using [Chromedp](https://github.com/chromedp) as browser-in-browser instead of http-proxy. The initial version was [less than 100 lines of code](https://gist.github.com/tenox7/b0f03c039b0a8b67f6c1bf47e2dd0df0).
* Version 4.0 has been completely refactored to use mouse clicks via imagemap instead parsing a href nodes. 
* Version 4.1 added sending keystrokes in to input boxes. You can now login to Gmail. Also now runs as a Docker container and on Cloud Run/Azure Containers.
* Version 4.5 introduces rendering whole pages in to a single tall image with client scrolling.
* Version 4.6 adds blazing fast gif encoding by [Hill Ma](https://github.com/mahiuchun).

## Credits

* Uses [chromedp](https://github.com/chromedp), thanks to [mvdan](https://github.com/mvdan) for dealing with my issues
* Uses [go-quantize](https://github.com/ericpauley/go-quantize), thanks to [ericpauley](https://github.com/ericpauley) for developing the missing go quantizer
* Thanks to Jason Stevens of [Fun With Virtualization](https://virtuallyfun.com/) for graciously hosting my rumblings
* Thanks to [claunia](https://github.com/claunia/) for help with the Python/Webkit version in the past
* Thanks to [Hill Ma](https://github.com/mahiuchun) for ultra fast gif encoding algorithm
* Historical Python/Webkit versions and prior art can be seen in [wrp-old](https://github.com/tenox7/wrp-old) repo

## Legal Stuff

License: Apache 2.0  
Copyright (c) 2013-2018 Antoni Sawicki  
Copyright (c) 2019-2022 Google LLC
