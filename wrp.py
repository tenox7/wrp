#!/usr/bin/env python2.7

# wrp.py - Web Rendering Proxy
# A HTTP proxy service that renders the requested URL in to a image associated
# with an imagemap of clickable links. This is an adaptation of previous works by
# picidae.net and Paul Hammond.

__version__ = "2.0"

#
# This program is based on the software picidae.py from picidae.net
# It was modified by Antoni Sawicki http://www.tenox.net/out/#wrp
#
# This program is based on the software webkit2png from Paul Hammond.
# It was extended by picidae.net
#
# Copyright (c) 2013-2014 Antoni Sawicki
# Copyright (c) 2012-2013 picidae.net
# Copyright (c) 2004-2013 Paul Hammond
# Copyright (c) 2017      Natalia Portillo
#
# Permission is hereby granted, free of charge, to any person obtaining a copy
# of this software and associated documentation files (the "Software"), to deal
# in the Software without restriction, including without limitation the rights
# to use, copy, modify, merge, publish, distribute, sublicense, and/or sell
# copies of the Software, and to permit persons to whom the Software is
# furnished to do so, subject to the following conditions:
#
# The above copyright notice and this permission notice shall be included in
# all copies or substantial portions of the Software.
#
# THE SOFTWARE IS PROVIDED "AS IS", WITHOUT WARRANTY OF ANY KIND, EXPRESS OR
# IMPLIED, INCLUDING BUT NOT LIMITED TO THE WARRANTIES OF MERCHANTABILITY,
# FITNESS FOR A PARTICULAR PURPOSE AND NONINFRINGEMENT. IN NO EVENT SHALL THE
# AUTHORS OR COPYRIGHT HOLDERS BE LIABLE FOR ANY CLAIM, DAMAGES OR OTHER
# LIABILITY, WHETHER IN AN ACTION OF CONTRACT, TORT OR OTHERWISE, ARISING FROM,
# OUT OF OR IN CONNECTION WITH THE SOFTWARE OR THE USE OR OTHER DEALINGS IN
# THE SOFTWARE.
#

# Configuration options:
PORT   = 8080
WIDTH  = 1024
HEIGHT = 768
ISMAP  = "true"
WAIT    = 1  # sleep for 1 second to allow javascript renders
QUALITY = 75 # For JPEG: image quality 0-100; For PNG: sets compression level (leftmost digit 0 fastest, 9 best)
AUTOWIDTH = True # Check for browser width using javascript
FORMAT = "AUTO" # AUTO = GIF for mac OS, JPG for rest; PNG, GIF, JPG as supported values.

# PythonMagick configuration options
MK_MONOCHROME = False # Convert the render to a black and white dithered image
MK_GRAYSCALE = False # Convert the render to a grayscal dithered image
MK_COLORS = 0 # Reduce number of colors in the image. 0 for not reducing. Less than 256 works in grayscale also.
MK_DITHER = False # Dither the image to reduce size. GIFs will always be dithered. Ignored if MK_COLORS is not set.

import re
import random
import os
import time
import string
import urllib
import socket
import SocketServer
import SimpleHTTPServer
import threading
import Queue
import sys
import logging
import StringIO

try:
    import PythonMagick
    HasMagick = True
except ImportError:
    HasMagick = False

# Request queue (URLs go in here)
REQ = Queue.Queue()
# Response queue (dummy response objects)
RESP = Queue.Queue()
# Renders dictionary
RENDERS = {}

#######################
### Linux CODEPATH ###
#######################

if sys.platform == "linux" or sys.platform == "linux2":
    try:
        from PyQt5.QtCore import *
        from PyQt5.QtGui import *
        from PyQt5.QtWebKit import *
        from PyQt5.QtWebKitWidgets import *
        from PyQt5.QtNetwork import *
        from PyQt5.QtWidgets import *
        IsPyQt5 = True
    except ImportError:
        from PyQt4.QtCore import *
        from PyQt4.QtGui import *
        from PyQt4.QtWebKit import *
        from PyQt4.QtNetwork import *
        IsPyQt5 = False

    # claunia: Check how to use this in macOS
    logging.basicConfig(filename='/dev/stdout', level=logging.WARN, )
    logger = logging.getLogger('wrp')

    # Class for Website-Rendering. Uses QWebPage, which
    # requires a running QtGui to work.
    class WebkitRenderer(QObject):
        def __init__(self, **kwargs):
            """Sets default values for the properties."""

            if not QApplication.instance():
                raise RuntimeError(self.__class__.__name__ + \
                " requires a running QApplication instance")
            QObject.__init__(self)

            # Initialize default properties
            self.width = kwargs.get('width', 0)
            self.height = kwargs.get('height', 0)
            self.timeout = kwargs.get('timeout', 0)
            self.wait = kwargs.get('wait', 0)
            self.logger = kwargs.get('logger', None)
            # Set this to true if you want to capture flash.
            # Not that your desktop must be large enough for
            # fitting the whole window.
            self.grabWholeWindow = kwargs.get('grabWholeWindow', False)

            # Set some default options for QWebPage
            self.qWebSettings = {
                QWebSettings.JavascriptEnabled : True,
                QWebSettings.PluginsEnabled : True,
                QWebSettings.PrivateBrowsingEnabled : True,
                QWebSettings.JavascriptCanOpenWindows : False
            }

        def render(self, url):
            """Renders the given URL into a QImage object"""
            # We have to use this helper object because
            # QApplication.processEvents may be called, causing
            # this method to get called while it has not returned yet.
            helper = _WebkitRendererHelper(self)
            helper._window.resize(self.width, self.height)
            image = helper.render(url)

            # Bind helper instance to this image to prevent the
            # object from being cleaned up (and with it the QWebPage, etc)
            # before the data has been used.
            image.helper = helper

            return image

    class _WebkitRendererHelper(QObject):
        """This helper class is doing the real work. It is required to
        allow WebkitRenderer.render() to be called "asynchronously"
        (but always from Qt's GUI thread).
        """

        def __init__(self, parent):
            """Copies the properties from the parent (WebkitRenderer) object,
            creates the required instances of QWebPage, QWebView and QMainWindow
            and registers some Slots.
            """
            QObject.__init__(self)

            # Copy properties from parent
            for key, value in parent.__dict__.items():
                setattr(self, key, value)

            # Create and connect required PyQt4 objects
            self._page = CustomWebPage(logger=self.logger)
            self._view = QWebView()
            self._view.setPage(self._page)
            self._window = QMainWindow()
            self._window.setCentralWidget(self._view)

            # Import QWebSettings
            for key, value in self.qWebSettings.iteritems():
                self._page.settings().setAttribute(key, value)

            # Connect required event listeners
            if IsPyQt5:
                self._page.loadFinished.connect(self._on_load_finished)
                self._page.loadStarted.connect(self._on_load_started)
                self._page.networkAccessManager().sslErrors.connect(self._on_ssl_errors)
                self._page.networkAccessManager().finished.connect(self._on_each_reply)
            else:
                self.connect(self._page, SIGNAL("loadFinished(bool)"), self._on_load_finished)
                self.connect(self._page, SIGNAL("loadStarted()"), self._on_load_started)
                self.connect(self._page.networkAccessManager(),
                            SIGNAL("sslErrors(QNetworkReply *,const QList<QSslError>&)"),
                            self._on_ssl_errors)
                self.connect(self._page.networkAccessManager(),
                            SIGNAL("finished(QNetworkReply *)"),
                            self._on_each_reply)

            # The way we will use this, it seems to be unesseccary to have Scrollbars enabled
            self._page.mainFrame().setScrollBarPolicy(Qt.Horizontal, Qt.ScrollBarAlwaysOff)
            self._page.mainFrame().setScrollBarPolicy(Qt.Vertical, Qt.ScrollBarAlwaysOff)
            self._page.settings().setUserStyleSheetUrl(
                QUrl("data:text/css,html,body{overflow-y:hidden !important;}"))

            # Show this widget
            # self._window.show()

        def __del__(self):
            """Clean up Qt4 objects. """
            self._window.close()
            del self._window
            del self._view
            del self._page

        def render(self, url):
            """The real worker. Loads the page (_load_page) and awaits
            the end of the given 'delay'. While it is waiting outstanding
            QApplication events are processed.
            After the given delay, the Window or Widget (depends
            on the value of 'grabWholeWindow' is drawn into a QPixmap
            """
            self._load_page(url, self.width, self.height, self.timeout)
            # Wait for end of timer. In this time, process
            # other outstanding Qt events.
            if self.wait > 0:
                if self.logger: self.logger.debug("Waiting %d seconds " % self.wait)
                waitToTime = time.time() + self.wait
                while time.time() < waitToTime:
                    if QApplication.hasPendingEvents():
                        QApplication.processEvents()

            if self.grabWholeWindow:
                # Note that this does not fully ensure that the
                # window still has the focus when the screen is
                # grabbed. This might result in a race condition.
                self._view.activateWindow()
                if IsPyQt5:
                    image = QScreen.grabWindow(self._window.winId())
                else:
                    image = QPixmap.grabWindow(self._window.winId())
            else:
                if IsPyQt5:
                    image = QWidget.grab(self._window)
                else:
                    image = QPixmap.grabWidget(self._window)

            httpout = WebkitRenderer.httpout

            frame = self._view.page().currentFrame()
            web_url = frame.url().toString()

            # Write URL map
            httpout.write("<!-- Web Rendering Proxy v%s by Antoni Sawicki -->\n"
                          % (__version__))
            httpout.write("<!-- Request for [%s] frame [%s] -->\n"
                          % (WebkitRenderer.req_url, web_url))
            # Get title
            httpout.write("<HTML><HEAD>")
            for ttl in frame.findAllElements('title'):
                httpout.write((u"<TITLE>%s</TITLE>"
                              % ttl.toPlainText()).encode('utf-8', errors='ignore'))
                break # Don't repeat bad HTML coding with several title marks
            httpout.write("</HEAD>\n<BODY>\n")

            if AUTOWIDTH:
                httpout.write("<script>document.write('<span style=\"display: none;\"><img src=\"http://width-' + document.body.clientWidth + '-px.jpg\" width=\"0\" height=\"0\"></span>');</script>\n")

            if ISMAP == "true":
                httpout.write("<A HREF=\"http://%s\">"
                              "<IMG SRC=\"http://%s\" ALT=\"wrp-render\" ISMAP>\n"
                              "</A>\n" % (WebkitRenderer.req_map, WebkitRenderer.req_img))
                mapfile = StringIO.StringIO()
                mapfile.write("default %s\n" % (web_url))
            else:
                httpout.write("<IMG SRC=\"http://%s\" ALT=\"wrp-render\" USEMAP=\"#map\">\n"
                              "<MAP NAME=\"map\">\n" % (WebkitRenderer.req_img))

            for x in frame.findAllElements('a'):
                turl = QUrl(web_url).resolved(QUrl(x.attribute('href'))).toString()
                xmin, ymin, xmax, ymax = x.geometry().getCoords()
                if ISMAP == "true":
                    mapfile.write("rect %s %i,%i %i,%i\n".decode('utf-8', errors='ignore') % (turl, xmin, ymin, xmax, ymax))
                else:
                    httpout.write("<AREA SHAPE=\"RECT\""
                                  " COORDS=\"%i,%i,%i,%i\""
                                  " ALT=\"%s\" HREF=\"%s\">\n".decode('utf-8', errors='ignore')
                                  % (xmin, ymin, xmax, ymax, turl, turl))

            if ISMAP != "true":
                httpout.write("</MAP>\n")

            httpout.write("</BODY>\n</HTML>\n")

            if ISMAP == "true":
                RENDERS[WebkitRenderer.req_map] = mapfile

            return image

        def _load_page(self, url, width, height, timeout):
            """
            This method implements the logic for retrieving and displaying
            the requested page.
            """

            # This is an event-based application. So we have to wait until
            # "loadFinished(bool)" raised.
            cancelAt = time.time() + timeout
            self.__loading = True
            self.__loadingResult = False # Default
            self._page.mainFrame().load(QUrl(url))
            while self.__loading:
                if timeout > 0 and time.time() >= cancelAt:
                    raise RuntimeError("Request timed out on %s" % url)
                while QApplication.hasPendingEvents() and self.__loading:
                    QCoreApplication.processEvents()

            if self.logger: self.logger.debug("Processing result")

            if self.__loading_result == False:
                if self.logger: self.logger.warning("Failed to load %s" % url)

            # Set initial viewport (the size of the "window")
            size = self._page.mainFrame().contentsSize()
            if self.logger: self.logger.debug("contentsSize: %s", size)
            if width > 0:
                size.setWidth(width)
            if height > 0:
                size.setHeight(height)

            self._window.resize(size)

        def _on_each_reply(self, reply):
            """Logs each requested uri"""
            self.logger.debug("Received %s" % (reply.url().toString()))

        # Eventhandler for "loadStarted()" signal
        def _on_load_started(self):
            """Slot that sets the '__loading' property to true."""
            if self.logger: self.logger.debug("loading started")
            self.__loading = True

        # Eventhandler for "loadFinished(bool)" signal
        def _on_load_finished(self, result):
            """Slot that sets the '__loading' property to false and stores
            the result code in '__loading_result'.
            """
            if self.logger: self.logger.debug("loading finished with result %s", result)
            self.__loading = False
            self.__loading_result = result

        # Eventhandler for "sslErrors(QNetworkReply *,const QList<QSslError>&)" signal
        def _on_ssl_errors(self, reply, errors):
            """Slot that writes SSL warnings into the log but ignores them."""
            for e in errors:
                if self.logger: self.logger.warn("SSL: " + e.errorString())
            reply.ignoreSslErrors()

    class CustomWebPage(QWebPage):
        def __init__(self, **kwargs):
            super(CustomWebPage, self).__init__()
            self.logger = kwargs.get('logger', None)

        def javaScriptAlert(self, frame, message):
            if self.logger: self.logger.debug('Alert: %s', message)

        def javaScriptConfirm(self, frame, message):
            if self.logger: self.logger.debug('Confirm: %s', message)
            return False

        def javaScriptPrompt(self, frame, message, default, result):
            """This function is called whenever a JavaScript program running inside frame tries to
            prompt the user for input. The program may provide an optional message, msg, as well
            as a default value for the input in defaultValue.

            If the prompt was cancelled by the user the implementation should return false;
            otherwise the result should be written to result and true should be returned.
            If the prompt was not cancelled by the user, the implementation should return true and
            the result string must not be null.
            """
            if self.logger: self.logger.debug('Prompt: %s (%s)' % (message, default))
            return False

        def shouldInterruptJavaScript(self):
            """This function is called when a JavaScript program is running for a long period of
            time. If the user wanted to stop the JavaScript the implementation should return
            true; otherwise false.
            """
            if self.logger: self.logger.debug("WebKit ask to interrupt JavaScript")
            return True

    #===============================================================================

    def init_qtgui(display=None, style=None, qtargs=None):
        """Initiates the QApplication environment using the given args."""
        if QApplication.instance():
            logger.debug("QApplication has already been instantiated. \
                            Ignoring given arguments and returning existing QApplication.")
            return QApplication.instance()

        qtargs2 = [sys.argv[0]]

        if display:
            qtargs2.append('-display')
            qtargs2.append(display)
            # Also export DISPLAY var as this may be used
            # by flash plugin
            os.environ["DISPLAY"] = display

        if style:
            qtargs2.append('-style')
            qtargs2.append(style)

        qtargs2.extend(qtargs or [])

        return QApplication(qtargs2)

    # Technically, this is a QtGui application, because QWebPage requires it
    # to be. But because we will have no user interaction, and rendering can
    # not start before 'app.exec_()' is called, we have to trigger our "main"
    # by a timer event.
    def __main_qt():
        # Render the page.
        # If this method times out or loading failed, a
        # RuntimeException is thrown
        try:
            while True:
                req = REQ.get()
                WebkitRenderer.httpout = req[0]
                WebkitRenderer.req_url = req[1]
                WebkitRenderer.req_img = req[2]
                WebkitRenderer.req_map = req[3]
                if WebkitRenderer.req_url == "http://wrp.stop/" or WebkitRenderer.req_url == "http://www.wrp.stop/":
                    print ">>> Terminate Request Received"
                    QApplication.exit(0)
                    break

                # Initialize WebkitRenderer object
                renderer = WebkitRenderer()
                renderer.logger = logger
                renderer.width = WIDTH
                renderer.height = HEIGHT
                renderer.timeout = 60
                renderer.wait = WAIT
                renderer.grabWholeWindow = False

                image = renderer.render(WebkitRenderer.req_url)
                qBuffer = QBuffer()

                if HasMagick:
                    image.save(qBuffer, 'png', QUALITY)
                    blob = PythonMagick.Blob(qBuffer.buffer().data())
                    mimg = PythonMagick.Image(blob)
                    mimg.quality(QUALITY)

                    if FORMAT=="GIF" and not MK_MONOCHROME and not MK_GRAYSCALE and not MK_DITHER and MK_COLORS != 0 and not MK_COLORS <= 256:
                        mimg.quantizeColors(256)
                        mimg.quantizeDither()
                        mimg.quantize()

                    if MK_MONOCHROME:
                        mimg.quantizeColorSpace(PythonMagick.ColorspaceType.GRAYColorspace)
                        mimg.quantizeColors(2)
                        mimg.quantizeDither()
                        mimg.quantize()
                        mimg.monochrome()
                    elif MK_GRAYSCALE:
                        mimg.quantizeColorSpace(PythonMagick.ColorspaceType.GRAYColorspace)
                        if MK_COLORS > 0 and MK_COLORS < 256:
                            mimg.quantizeColors(MK_COLORS)
                        else:
                            mimg.quantizeColors(256)
                        mimg.quantizeDither()
                        mimg.quantize()
                    else:
                        if MK_COLORS > 0:
                            mimg.quantizeColors(MK_COLORS)
                            if MK_DITHER:
                                mimg.quantizeDither()
                            mimg.quantize()

                    if FORMAT=="AUTO" or FORMAT=="JPG":
                        mimg.write(blob, "jpg")
                    elif FORMAT=="PNG":
                        mimg.write(blob, "png")
                    elif FORMAT=="GIF":
                        mimg.write(blob, "gif")
                    output = StringIO.StringIO()
                    output.write(blob.data)
                else:
                    if FORMAT=="AUTO" or FORMAT=="JPG":
                        image.save(qBuffer, 'jpg', QUALITY)
                    elif FORMAT=="PNG":
                        image.save(qBuffer, 'png', QUALITY)

                    output = StringIO.StringIO()
                    output.write(qBuffer.buffer().data())

                RENDERS[req[2]] = output

                del renderer
                print ">>> done: %s [%d kb]..." % (WebkitRenderer.req_img, output.len/1024)

                RESP.put('')

            QApplication.exit(0)
        except RuntimeError, e:
            logger.error("main: %s" % e)
            print >> sys.stderr, e
            QApplication.exit(1)

######################
### macOS CODEPATH ###
######################

elif sys.platform == "darwin":
    import Foundation
    import WebKit
    import AppKit
    import objc

    class AppDelegate(Foundation.NSObject):
        # what happens when the app starts up
        def applicationDidFinishLaunching_(self, aNotification):
            webview = aNotification.object().windows()[0].contentView()
            webview.frameLoadDelegate().getURL(webview)

    class WebkitLoad(Foundation.NSObject, WebKit.protocols.WebFrameLoadDelegate):
        # what happens if something goes wrong while loading
        def webView_didFailLoadWithError_forFrame_(self, webview, error, frame):
            if error.code() == Foundation.NSURLErrorCancelled:
                return
            print " ... something went wrong 1: " + error.localizedDescription()
            AppKit.NSApplication.sharedApplication().terminate_(None)

        def webView_didFailProvisionalLoadWithError_forFrame_(self, webview, error, frame):
            if error.code() == Foundation.NSURLErrorCancelled:
                return
            print " ... something went wrong 2: " + error.localizedDescription()
            AppKit.NSApplication.sharedApplication().terminate_(None)

        def getURL(self, webview):
            req = REQ.get()
            WebkitLoad.httpout = req[0]
            WebkitLoad.req_url = req[1]
            WebkitLoad.req_img = req[2]
            WebkitLoad.req_map = req[3]

            if WebkitLoad.req_url == "http://wrp.stop/" or WebkitLoad.req_url == "http://www.wrp.stop/":
                print ">>> Terminate Request Received"
                AppKit.NSApplication.sharedApplication().terminate_(None)

            nsurl = Foundation.NSURL.URLWithString_(WebkitLoad.req_url)
            if not (nsurl and nsurl.scheme()):
                nsurl = Foundation.NSURL.alloc().initFileURLWithPath_(WebkitLoad.req_url)
            nsurl = nsurl.absoluteURL()

            Foundation.NSURLRequest.setAllowsAnyHTTPSCertificate_forHost_(objc.YES, nsurl.host())

            self.resetWebview(webview)
            webview.mainFrame().loadRequest_(Foundation.NSURLRequest.requestWithURL_(nsurl))
            if not webview.mainFrame().provisionalDataSource():
                print " ... not a proper url?"
                RESP.put('')
                self.getURL(webview)

        def resetWebview(self, webview):
            rect = Foundation.NSMakeRect(0, 0, WIDTH, HEIGHT)
            webview.window().setContentSize_((WIDTH, HEIGHT))
            webview.setFrame_(rect)

        def captureView(self, view):
            view.window().display()
            view.window().setContentSize_(view.bounds().size)
            view.setFrame_(view.bounds())

            if hasattr(view, "bitmapImageRepForCachingDisplayInRect_"):
                bitmapdata = view.bitmapImageRepForCachingDisplayInRect_(view.bounds())
                view.cacheDisplayInRect_toBitmapImageRep_(view.bounds(), bitmapdata)
            else:
                view.lockFocus()
                bitmapdata = AppKit.NSBitmapImageRep.alloc()
                bitmapdata.initWithFocusedViewRect_(view.bounds())
                view.unlockFocus()
            return bitmapdata

        # what happens when the page has finished loading
        def webView_didFinishLoadForFrame_(self, webview, frame):
            # don't care about subframes
            if frame == webview.mainFrame():
                view = frame.frameView().documentView()

                output = StringIO.StringIO()

                if HasMagick:
                    output.write(self.captureView(view).representationUsingType_properties_(
                        AppKit.NSPNGFileType, None))
                    blob = PythonMagick.Blob(output)
                    mimg = PythonMagick.Image(blob)
                    mimg.quality(QUALITY)

                    if FORMAT=="GIF" and not MK_MONOCHROME and not MK_GRAYSCALE and not MK_DITHER and MK_COLORS != 0 and not MK_COLORS <= 256:
                        mimg.quantizeColors(256)
                        mimg.quantizeDither()
                        mimg.quantize()

                    if MK_MONOCHROME:
                        mimg.quantizeColorSpace(PythonMagick.ColorspaceType.GRAYColorspace)
                        mimg.quantizeColors(2)
                        mimg.quantizeDither()
                        mimg.quantize()
                        mimg.monochrome()
                    elif MK_GRAYSCALE:
                        mimg.quantizeColorSpace(PythonMagick.ColorspaceType.GRAYColorspace)
                        if MK_COLORS > 0 and MK_COLORS < 256:
                            mimg.quantizeColors(MK_COLORS)
                        else:
                            mimg.quantizeColors(256)
                        mimg.quantizeDither()
                        mimg.quantize()
                    else:
                        if MK_COLORS > 0:
                            mimg.quantizeColors(MK_COLORS)
                            if MK_DITHER:
                                mimg.quantizeDither()
                            mimg.quantize()

                    if FORMAT=="JPG":
                        mimg.write(blob, "jpg")
                    elif FORMAT=="PNG":
                        mimg.write(blob, "png")
                    elif FORMAT=="AUTO" or FORMAT=="GIF":
                        mimg.write(blob, "gif")
                    output = StringIO.StringIO()
                    output.write(blob.data)
                else:
                    if FORMAT=="AUTO" or FORMAT=="GIF":
                        output.write(self.captureView(view).representationUsingType_properties_(
                            AppKit.NSGIFFileType, None))
                    elif FORMAT=="JPG":
                        output.write(self.captureView(view).representationUsingType_properties_(
                            AppKit.NSJPEGFileType, None))
                    elif FORMAT=="PNG":
                        output.write(self.captureView(view).representationUsingType_properties_(
                            AppKit.NSPNGFileType, None))

                RENDERS[WebkitLoad.req_img] = output

                # url of the rendered page
                web_url = frame.dataSource().initialRequest().URL().absoluteString()

                httpout = WebkitLoad.httpout

                httpout.write("<!-- Web Rendering Proxy v%s by Antoni Sawicki -->\n"
                              % (__version__))
                httpout.write("<!-- Request for [%s] frame [%s] -->\n"
                              % (WebkitLoad.req_url, web_url))

                domdocument = frame.DOMDocument()
                # Get title
                httpout.write("<HTML><HEAD>")
                httpout.write((u"<TITLE>%s</TITLE>"
                                % domdocument.title()).encode('utf-8', errors='ignore'))
                httpout.write("</HEAD>\n<BODY>\n")

                if AUTOWIDTH:
                    httpout.write("<script>document.write('<span style=\"display: none;\"><img src=\"http://width-' + document.body.clientWidth + '-px.jpg\" width=\"0\" height=\"0\"></span>');</script>\n")

                if ISMAP == "true":
                    httpout.write("<A HREF=\"http://%s\">"
                                  "<IMG SRC=\"http://%s\" ALT=\"wrp-render\" ISMAP>\n"
                                  "</A>\n" % (WebkitLoad.req_map, WebkitLoad.req_img))
                    mapfile = StringIO.StringIO()
                    mapfile.write("default %s\n" % (web_url))
                else:
                    httpout.write("<IMG SRC=\"http://%s\" ALT=\"wrp-render\" USEMAP=\"#map\">\n"
                                  "<MAP NAME=\"map\">\n" % (WebkitLoad.req_img))

                domnodelist = domdocument.getElementsByTagName_('A')
                i = 0
                while  i < domnodelist.length():
                    turl = domnodelist.item_(i).valueForKey_('href')
                    #TODO: crashes? validate url? insert web_url if wrong?
                    myrect = domnodelist.item_(i).boundingBox()

                    xmin = Foundation.NSMinX(myrect)
                    ymin = Foundation.NSMinY(myrect)
                    xmax = Foundation.NSMaxX(myrect)
                    ymax = Foundation.NSMaxY(myrect)

                    if ISMAP == "true":
                        mapfile.write("rect %s %i,%i %i,%i\n".decode('utf-8', errors='ignore') % (turl, xmin, ymin, xmax, ymax))
                    else:
                        httpout.write("<AREA SHAPE=\"RECT\""
                                      " COORDS=\"%i,%i,%i,%i\""
                                      " ALT=\"%s\" HREF=\"%s\">\n".decode('utf-8', errors='ignore')
                                      % (xmin, ymin, xmax, ymax, turl, turl))

                    i += 1

                if ISMAP != "true":
                    httpout.write("</MAP>\n")

                httpout.write("</BODY>\n</HTML>\n")

                if ISMAP == "true":
                    RENDERS[WebkitLoad.req_map] = mapfile

                # Return to Proxy thread and Loop...
                RESP.put('')
                self.getURL(webview)

    def main_cocoa():
        # Launch NS Application
        AppKit.NSApplicationLoad()
        app = AppKit.NSApplication.sharedApplication()
        delegate = AppDelegate.alloc().init()
        AppKit.NSApp().setDelegate_(delegate)
        AppKit.NSBundle.mainBundle().infoDictionary()['NSAppTransportSecurity'] = \
            dict(NSAllowsArbitraryLoads=True)
        rect = Foundation.NSMakeRect(-16000, -16000, 100, 100)
        win = AppKit.NSWindow.alloc()
        win.initWithContentRect_styleMask_backing_defer_(rect, AppKit.NSBorderlessWindowMask, 2, 0)
        webview = WebKit.WebView.alloc()
        webview.initWithFrame_(rect)
        webview.mainFrame().frameView().setAllowsScrolling_(objc.NO)
        webkit_version = Foundation.NSBundle.bundleForClass_(WebKit.WebView). \
                         objectForInfoDictionaryKey_(WebKit.kCFBundleVersionKey)[1:]
        webview.setApplicationNameForUserAgent_("Like-Version/6.0 Safari/%s wrp/%s"
                                                % (webkit_version, __version__))
        win.setContentView_(webview)
        loaddelegate = WebkitLoad.alloc().init()
        loaddelegate.options = [""]
        webview.setFrameLoadDelegate_(loaddelegate)
        app.run()

#######################
### COMMON CODEPATH ###
#######################
class Proxy(SimpleHTTPServer.SimpleHTTPRequestHandler):
    def do_GET(self):
        req_url = self.path
        httpout = self.wfile

        map_re = re.match(r"http://(wrp-\d+\.map).*?(\d+),(\d+)", req_url)
        wid_re = re.match(r"http://(width-[0-9]+-px\.jpg).*", req_url)
        gif_re = re.match(r"http://(wrp-\d+\.gif).*", req_url)
        jpg_re = re.match(r"http://(wrp-\d+\.jpg).*", req_url)
        png_re = re.match(r"http://(wrp-\d+\.png).*", req_url)

        # Serve Rendered GIF
        if gif_re:
            img = gif_re.group(1)
            print ">>> request for rendered gif image... %s  [%d kb]" \
                   % (img, RENDERS[img].len/1024)
            self.send_response(200, 'OK')
            self.send_header('Content-type', 'image/gif')
            self.end_headers()
            httpout.write(RENDERS[img].getvalue())
            del RENDERS[img]

        elif jpg_re:
            img = jpg_re.group(1)
            print ">>> request for rendered jpg image... %s  [%d kb]" \
                   % (img, RENDERS[img].len/1024)
            self.send_response(200, 'OK')
            self.send_header('Content-type', 'image/jpeg')
            self.end_headers()
            httpout.write(RENDERS[img].getvalue())
            del RENDERS[img]

        elif png_re:
            img = png_re.group(1)
            print ">>> request for rendered png image... %s  [%d kb]" \
                   % (img, RENDERS[img].len/1024)
            self.send_response(200, 'OK')
            self.send_header('Content-type', 'image/png')
            self.end_headers()
            httpout.write(RENDERS[img].getvalue())
            del RENDERS[img]

        elif wid_re:
            global WIDTH
            try:
                wid = req_url.split("-")
                WIDTH = int(wid[1])
                print ">>> width request: %d" % WIDTH
            except:
                print ">>> width request error" % WIDTH

            self.send_error(404, "Width request")
            self.end_headers()

        # Process ISMAP Request
        elif map_re:
            map = map_re.group(1)
            req_x = int(map_re.group(2))
            req_y = int(map_re.group(3))
            print ">>> ISMAP request... %s [%d,%d] " % (map, req_x, req_y)

            mapf = RENDERS[map]
            mapf.seek(0)
            goto_url = "none"
            for line in mapf.readlines():
                if re.match(r"(\S+)", line).group(1) == "default":
                    default_url = re.match(r"\S+\s+(\S+)", line).group(1)

                elif re.match(r"(\S+)", line).group(1) == "rect":
                    try:
                        rect = re.match(r"(\S+)\s+(\S+)\s+(\d+),(\d+)\s+(\d+),(\d+)", line)
                        min_x = int(rect.group(3))
                        min_y = int(rect.group(4))
                        max_x = int(rect.group(5))
                        max_y = int(rect.group(6))
                        if (req_x >= min_x) and \
                            (req_x <= max_x) and \
                            (req_y >= min_y) and \
                            (req_y <= max_y):
                            goto_url = rect.group(2)
                    except AttributeError:
                        pass

            if goto_url == "none":
                goto_url = default_url

            print ">>> ISMAP redirect: %s\n" % (goto_url)

            self.send_response(302, "Found")
            self.send_header("Location", goto_url)
            self.send_header("Content-type", "text/html")
            self.end_headers()
            httpout.write("<HTML><BODY><A HREF=\"%s\">%s</A></BODY></HTML>\n"
                          % (goto_url, goto_url))

        # Process a web page request and generate image
        else:
            print ">>> URL request... " + req_url

            if req_url == "http://wrp.stop/" or req_url == "http://www.wrp.stop/":
                REQ.put((httpout, req_url, "", ""))
                RESP.get()
            else:
                reqst = urllib.urlopen(req_url)

                if reqst.info().type == "text/html" or reqst.info().type == "application/xhtml+xml":
                # If an error occurs, send error headers to the requester
                    if reqst.getcode() >= 400:
                        self.send_response(reqst.getcode())
                        for hdr in reqst.info():
                            self.send_header(hdr, reqst.info()[hdr])
                        self.end_headers()
                    else:
                        self.send_response(200, 'OK')
                        self.send_header('Content-type', 'text/html')
                        self.end_headers()

                    rnd = random.randrange(0, 1000)

                    if FORMAT == "GIF":
                        req_extension = ".gif"
                    elif FORMAT == "JPG":
                        req_extension = ".jpg"
                    elif FORMAT == "PNG":
                        req_extension = ".png"
                    elif (sys.platform == "linux" or sys.platform == "linux2") and FORMAT == "AUTO":
                        req_extension = ".jpg"
                    elif sys.platform == "darwin" and FORMAT == "AUTO":
                        req_extension = ".gif"

                    req_img = "wrp-%s%s" % (rnd, req_extension)
                    req_map = "wrp-%s.map" % (rnd)

                    # To WebKit Thread
                    REQ.put((httpout, req_url, req_img, req_map))
                    # Wait for completition
                    RESP.get()
                # If the requested file is not HTML or XHTML, just return it as is.
                else:
                    self.send_response(reqst.getcode())
                    for hdr in reqst.info():
                        self.send_header(hdr, reqst.info()[hdr])
                    self.end_headers()
                    httpout.write(reqst.read())

def run_proxy():
    httpd = SocketServer.TCPServer(('', PORT), Proxy)
    print "Web Rendering Proxy v%s serving at port: %s" % (__version__, PORT)
    while 1:
        httpd.serve_forever()

def main():
    if(FORMAT != "AUTO" and FORMAT != "GIF" and FORMAT != "JPG" and FORMAT != "PNG"):
        sys.exit("Unsupported image format \"%s\". Exiting." % FORMAT)

    if (sys.platform == "linux" or sys.platform == "linux2") and FORMAT == "GIF" and not HasMagick:
        sys.exit("GIF format is not supported on this platform. Exiting.")

    # Launch Proxy Thread
    threading.Thread(target=run_proxy).start()

    if sys.platform == "linux" or sys.platform == "linux2":
        import signal
        try:
            import PyQt5.QtCore
        except ImportError:
            import PyQt4.QtCore
        # Initialize Qt-Application, but make this script
        # abortable via CTRL-C
        app = init_qtgui(display=None, style=None)
        signal.signal(signal.SIGINT, signal.SIG_DFL)

        QTimer.singleShot(0, __main_qt)
        sys.exit(app.exec_())
    elif sys.platform == "darwin":
        main_cocoa()
    else:
        sys.exit("Unsupported platform: %s. Exiting." % sys.platform)

if __name__ == '__main__': main()
