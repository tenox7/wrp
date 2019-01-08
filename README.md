# WRP - Web Rendering Proxy
A HTTP proxy server that allows to use historical and obsolete web browsers on the modern web. It works by rendering the web page in to a GIF/PNG/JPEG image associated with clickable imagemap of original web links.

New: Version 2.1 brings support for sslstrip to allow browsing https/SSL/TSL websites.


# Current Status
* It mostly works for casual browsing but the app is not very stable and your mileage may vary. 
* Secure aka https/SSL/TLS websites might work with use of `sslstrip`[1] cheat (enabled by default).
* Web form submission is not yet implemented.

## OS Support
WRP works on macOS (Mac OS X), Linux and FreeBSD. On macOS it uses Cocoa Webkit, on Linux/FreeBSD QT Webkit, for which needs PyQT4 or PyQT5.

## Installation
* macOS - should just work
* Linux/FreeBSD install `python-pyqt5.qtwebkit` and `sslstrip`
* For PythonMagick (Imagemagick library) install `python-pythonmagick`

## Configuration
Edit wrp.py, scroll past Copyright section to find config parameters

## Usage 
Configure your web browser to use HTTP proxy at IP address and port where WRP is running. If using browsers prior to HTML 3.2, ISMAP option may need to be enabled. Check configuration.

## More info and screenshots
* http://virtuallyfun.superglobalmegacorp.com/2014/03/11/web-rendering-proxy-update/
* http://virtuallyfun.superglobalmegacorp.com/2014/03/03/surfing-modern-web-with-ancient-browsers/

[1]: https://moxie.org/software/sslstrip/
