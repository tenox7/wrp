# WRP - Web Rendering Proxy

A HTTP proxy server that allows to use historical and obsolete web browsers on the modern web. It works by rendering the web page in to a GIF image associated with clickable imagemap of original web links.

## Current Status

* This is the new GoLang/ChdomeDP version.
* It's still lacking some features of the [older version](/old) (such as real http proxy mode and image manipulation) but far surpases it in terms of stability and usability. 
* Currently works as browser-in-browser however work on http proxy mode is under way.
* It's beta quality but I can actually fix and maintain the code.

## Todo

* Configurable color palete and quantization.
* Real http proxy support via [goproxy](https://github.com/elazarl/goproxy) - if you really need a real proxy, for now use the [old/](/old) version.
* Padded box model coordinates.
* Input boxes support. However today you can cheat by using headed mode and input your data on the WRP server.

## Flags
```
-l  listen address:port, default :8080
-h  headed mode, display browser window
-d  chromedp debug logging
```

## More info and screenshots
* http://virtuallyfun.superglobalmegacorp.com/2014/03/11/web-rendering-proxy-update/
* http://virtuallyfun.superglobalmegacorp.com/2014/03/03/surfing-modern-web-with-ancient-browsers/
