# WRP - Web Rendering Proxy

A browser-in-browser "proxy" server that allows to use historical / vintage web browsers on the modern web. It works by rendering a web page in to a GIF or PNG image with clickable imagemap.

![Internet Explorer 1.5 doing Gmail](wrp.png)

## Usage

1. [Download a WRP binary](https://github.com/tenox7/wrp/releases/) and run it on a machine that will become your WRP gateway/server. 
This machine should be pretty modern, high spec and Google Chrome / Chromium Browser is required to be preinstalled.
2. Point your legacy browser to `http://address:port` of WRP server. Do not set or use it as a "proxy server".
3. Type a search string or a http/https URL and click **Go**.
4. Adjust your screen width/height/scale/#colors to fit in your old browser.
5. Scroll web page by clicking on the in-image scroll bar.
6. Do not use client browser history-back, instead use **Bk** button in the app.
7. To send keystrokes, fill **K** input box and press **Go**. There also are buttons for backspace, enter and arrow keys.
8. You can set height **H** to `0` to render pages in to one tall image without the vertical scrollbar. This should not be used with old and low spec clients. Such images will be very large and take long time to encode/decode, especially for GIF.
9. Prefer PNG over GIF if your browser supports it. PNG is much faster, whereas GIF requires a lot of additional processing on both client and server.

## Docker

```shell
$ docker run -d -p 8080:8080 tenox7/wrp
```

## Google Cloud Run

```shell
$ gcloud run deploy --platform managed --image=gcr.io/tenox7/wrp:latest --memory=2Gi --args='-t=png','-g=1280x0x256'
```

Or from [Web UI](https://console.cloud.google.com/run). Use `gcr.io/tenox7/wrp` as container image URL.

Note that unfortunately GCR forces https. Your browser support of encryption protocols and certification authorities will vary. 

## Azure Container Instances

```shell
$ az container create --resource-group wrp --name wrp --image gcr.io/tenox7/wrp:latest --cpu 1 --memory 2 --ports 80 --protocol tcp --os-type Linux --ip-address Public --command-line '/wrp -l :80 -t png -g 1280x0x256'
```

Fortunately ACI allows port 80 without encryption.


## Flags

```
-l  listen address:port, default :8080
-t  image type gif (default) or png, when using PNG number of colors is ignored
-g  image geometry, WxHxC, height can be 0 for unlimited, default 1152x600x256
-h  headed mode, display browser window on the server
-d  chromedp debug logging
-n  do not free maps and gif images after use
```

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

* In 2014, version 1.0 started as a *cgi-bin* script, adaptation of `webkit2png.py` and `pcidade.py`, [blog post](https://virtuallyfun.com/2014/03/03/surfing-modern-web-with-ancient-browsers/).
* Later in 2014, version 2.0 became a stand alone http-proxy server, also supporting both Linux and MacOS, [another post](https://virtuallyfun.com/wordpress/2014/03/11/web-rendering-proxy-update//).
* In 2016 the whole internet migrated to HTTPS/SSL/TLS and WRP largely stopped working. Python code became unmaintainable and mostly unportable (especially to Windows, even WSL).
* In 2019 WRP 3.0 has been rewritten in Golang/Chromedp as browser-in-browser instead of http proxy.
* Later in 2019, WRP 4.0 has been completely refactored to use mouse clicks via imagemap instead parsing a href nodes. Also in 4.1 added sending keystrokes in to input boxes. You can now login to Gmail. Also now runs as a Docker container. Version 4.5 introduces rendering whole pages in to one tall image image.

## Credits

* Uses [chromedp](https://github.com/chromedp), thanks to [mvdan](https://github.com/mvdan) for dealing with my issues
* Uses [go-quantize](https://github.com/ericpauley/go-quantize), thanks to [ericpauley](https://github.com/ericpauley) for developing the missing go quantizer
* Thanks to Jason Stevens of [Fun With Virtualization](https://virtuallyfun.com/) for graciously hosting my rumblings
* Thanks to [claunia](https://github.com/claunia/) for help with the Python/Webkit version in the past

## Legal Stuff

License: Apache 2.0  
Copyright (c) 2013-2018 Antoni Sawicki  
Copyright (c) 2019-2020 Google LLC
