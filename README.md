# WRP - Web Rendering Proxy

A HTTP proxy server that allows to use historical and obsolete web browsers on the modern web. It works by rendering the web page in to a GIF image associated with clickable imagemap of original web links.

**You are looking at a GoLang / CDP branch of WRP.**

**This code is under active development and not fully usable yet.**

## Done so far

* basic browser-in-browser mode
* screenshot and serve image+map via CDP
* gif with Floyd–Steinberg dithering
* random image addressing
* resolve relative links
* paginated scrolling
* google search on input not starting with ^http

## Todo

* ISMAP - underway
* net/url: invalid control character in URL on Windows
* configurable color palete and quantization
* real http proxy support
* option to encode as png/jpeg
* padded box model coordinates
* better http server shutdown
* chromedp logging, timeout, non-headless flags

## Python version

Check [master branch](https://github.com/tenox7/wrp/tree/master) for "stable" Python-Webkit version.