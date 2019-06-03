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
* ISMAP, although for a redirect to work `-i` flag must be specified
  otherwise http-equiv refresh will be used and/or link provided

## Todo

* configurable color palete and quantization
* real http proxy support
* padded box model coordinates
* better http server shutdown
* chromedp logging, timeout, non-headless allocator flags

## Old Python version

Check [pywebkit/](/pywebkit) folder for the old Python-Webkit version.