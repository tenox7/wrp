# WRP - Web Rendering Proxy

A HTTP proxy server that allows to use historical and obsolete web browsers on the modern web. It works by rendering the web page in to a GIF image. It sends mouse clicks via ISMAP and keystrokes from a text box form input.

## Current Status

* This is a new reimplementation in GoLang/[ChromeDP](https://github.com/chromedp/chromedp).
* Beta but fully supported an maintained.
* Currently works as browser-in-browser. A real http proxy mode is being investigated. Check [issue #35](https://github.com/tenox7/wrp/issues/35) for updates.
* As of 4.1 supports clicking on non-link elements (eg. cookie warnings, dropdown menus, etc.) and sending keystrokes. Yes, you can login and use Gmail or play web based games from any old browser.

## Usage	

1. [Download a WRP binary](https://github.com/tenox7/wrp/releases/) and run it on a machine that will become your WRP server.
2. Point your legacy browser to `http://address:port` of WRP server. Do not set or use it as a "Proxy Server" (yet).
3. Type a search string or a http/https URL and click Go.	
4. Adjust your screen width/height/scale/#colors to fit in your old browser.	
5. Scroll web page by clicking on the in-image scroll bar.
6. Send keystrokes by filling in K input box and pressing Go. You also have buttons for backspace, enter and arrow keys.

![Internet Explorer 1.5 doing Gmail](wrp.png)

## Flags
```
-l  listen address:port, default :8080
-h  headed mode, display browser window
-d  chromedp debug logging
```

## More info and screenshots
* http://virtuallyfun.superglobalmegacorp.com/2014/03/11/web-rendering-proxy-update/
* http://virtuallyfun.superglobalmegacorp.com/2014/03/03/surfing-modern-web-with-ancient-browsers/

## Credits 
* Uses [chromedp](https://github.com/chromedp), thanks to [mvdan](https://github.com/mvdan) for dealing with my issues
* Uses [go-quantize](https://github.com/ericpauley/go-quantize), thanks to [ericpauley](https://github.com/ericpauley) for developing the missing go quantizer
* Thanks to Jason Stevens of [Fun With Virtualization](https://virtuallyfun.com/) for graciously hosting my rumblings

## Legal Stuff
License: Apache 2.0  
Copyright (c) 2013-2018 Antoni Sawicki  
Copyright (c) 2019 Google LLC
