# WRP - Web Rendering Proxy

A HTTP proxy server that allows to use historical / vintage web browsers on the modern web. It works by rendering a web page in to a GIF image with clickable ISMAP.

![Internet Explorer 1.5 doing Gmail](wrp.png)

## Current Status

* This is a new version using GoLang/[ChromeDP](https://github.com/chromedp/chromedp) (Python/Webkit is now deprecated).
* Fully supported an maintained.
* Works as browser-in-browser. A real http proxy mode is being investigated. Check [issue #35](https://github.com/tenox7/wrp/issues/35) for updates.
* Supports clicking on non-link elements (eg. cookie warnings, dropdown menus, etc.) and sending keystrokes. Yes, you can login to Gmail.

## Usage	

1. [Download a WRP binary](https://github.com/tenox7/wrp/releases/) and run it on a machine that will become your WRP gateway server.
2. Point your legacy browser to `http://address:port` of WRP server. Do not set or use it as a "Proxy Server" (yet).
3. Type a search string or a http/https URL and click Go.	
4. Adjust your screen width/height/scale/#colors to fit in your old browser.	
5. Scroll web page by clicking on the in-image scroll bar.
6. Do not use client browser history-back, instead use Bk WRP button.
7. To send keystrokes fill K input box and press Go. There also are buttons for backspace, enter and arrow keys.

## Docker

docker hub:

```
docker run -d -p 8080:8080 tenox7/wrp
```

gcr.io:

```
docker run -d -p 8080:8080 gcr.io/tenox7/wrp:latest
```

## Flags

```
-l  listen address:port, default :8080
-h  headed mode, display browser window
-d  chromedp debug logging
-n  do not free maps and gif images after use
-t  image type gif (default) or png, when using PNG number of colors is ignored
```

## Minimal Requirements

- Server / Gateway should run on a modern hardware/os that supports memory hungry Chrome.

- Client / Browser needs to support HTML FORMs and ISMAP. Typically Mosaic 2.0 would be minimum version for forms. However ISMAP was supported since 0.6B, so if you manually enter url using `?url=...` you can use ealier version.

## History
* In 2014, version 1.0 started as a cgi-bin script, adaptation of `webkit2png.py` and `pcidade.py`, [blog post](https://virtuallyfun.com/2014/03/03/surfing-modern-web-with-ancient-browsers/).
* Later in 2014, version 2.0 became a stand alone http-proxy server, also support for both Linux/MacOS, [another post](https://virtuallyfun.com/wordpress/2014/03/11/web-rendering-proxy-update//).
* In 2016 the whole internet migrated to HTTPS/SSL/TLS and WRP largely stopped working. Python code became unmaintainable and mostly unportable (especially to Windows, even WSL).
* In 2019 WRP 3.0 has been rewritten in Golang/Chromedp as browser-in-browser instead of http proxy.
* Later in 2019, WRP 4.0 has been completely refactored to use mouse clicks instead using a href nodes. Also in 4.1 added sending keystrokes in to input boxes. You can now login to Gmail. Also now runs as a Docker container.

## Credits 
* Uses [chromedp](https://github.com/chromedp), thanks to [mvdan](https://github.com/mvdan) for dealing with my issues
* Uses [go-quantize](https://github.com/ericpauley/go-quantize), thanks to [ericpauley](https://github.com/ericpauley) for developing the missing go quantizer
* Thanks to Jason Stevens of [Fun With Virtualization](https://virtuallyfun.com/) for graciously hosting my rumblings
* Thanks to [claunia](https://github.com/claunia/) for help with the Python/Webkit version in the past

## Legal Stuff
License: Apache 2.0  
Copyright (c) 2013-2018 Antoni Sawicki  
Copyright (c) 2019 Google LLC
