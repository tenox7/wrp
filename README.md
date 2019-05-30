# WRP - Web Rendering Proxy
A HTTP proxy server that allows to use historical and obsolete web browsers on the modern web. It works by rendering the web page in to a GIF image associated with clickable imagemap of original web links.

**You are looking at a GoLang / CDP branch of WRP.**

**This code is under active development and not fully usable yet.**


## Done so far
* basic browser-in-browser mode
* process and serve image+map via cdp
* gif with Floyd–Steinberg dithering
* random image addressing
* resolve relative links

## Todo
* configurable size and scale
* ISMAP
* configurable color palete and quantization
* paginated scrolling
* real http proxy support
* encode to png/jpeg option
* padded box model coordinates
* better http server shutdown
* chromedp logging, timeout, non-headless flags

Check [master branch](https://github.com/tenox7/wrp/tree/master) for "stable" Python-Webkit version.


