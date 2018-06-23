# WRP - Web Rendering Proxy
A HTTP proxy server that renders the web page in to a GIF/PNG/JPEG image associated with clickable imagemap of the original web links. It allows to use historical and obsolete web browsers on the modern web. It's still a work in progress but it's quite stable and usable for casual web browsing.

Version 2.0 brings support for PythonMagick (ImageMagick Library) that allows to optimize and reduce image size or covert to greyscale or bitmap for these cool computers without color displays.

## OS Support
WRP works on macOS (Mac OS X), Linux and FreeBSD. On macOS it uses Cocoa Webkit, on Linux/FreeBSD QT Webkit, for which needs PyQT4 or PyQT5.

## Installation
* macOS - should just work
* Linux/FreeBSD install `python-pyqt5.qtwebkit`

## Configuration
Edit wrp.py, scroll past Copyright nonsense to find config parameters

## Usage 
Configure your web browser to use HTTP proxy at IP address and port where WRP is running. If using browsers prior to HTML 3.2, ISMAP option may need to be enabled. Check configuration.

## More info and screenshots
* http://virtuallyfun.superglobalmegacorp.com/2014/03/11/web-rendering-proxy-update/
* http://virtuallyfun.superglobalmegacorp.com/2014/03/03/surfing-modern-web-with-ancient-browsers/
