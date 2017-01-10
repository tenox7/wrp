#!/usr/bin/env python

# Web Rendering Proxy (qt-webkit) by Antoni Sawicki - http://www.tenox.net/out/#wrp
# HTTP proxy service that renders the web page in to a JPEG image 
# associated with clickable imagemap of the original web links
# This version works only with QT-Webkit (eg.: Linux, BSD, others)
#
# This program is loosely based on the following software:
# Adam Nelson webkit2png: https://github.com/adamn/python-webkit2png
# Roland Tapken: http://www.blogs.uni-osnabrueck.de/rotapken/2008/12/03/create-screenshots-of-a-web-page-using-python-and-qtwebkit/
# picidae.py from picidae.net: https://code.google.com/p/phantomjs/issues/attachmentText?id=209&aid=2090003000&name=picidae.py
# Paul Hammond webkit2png: http://www.paulhammond.org/webkit2png/
# 
# Copyright (c) 2013-2014 Antoni Sawicki
# Copyright (c) 2012 picidae.net
# Copyright (c) 2008 Roland Tapken
# Copyright (c) 2004-2014 Paul Hammond
#
# This program is free software; you can redistribute it and/or
# modify it under the terms of the GNU General Public License
# as published by the Free Software Foundation; either version 2
# of the License, or (at your option) any later version.
#
# This program is distributed in the hope that it will be useful,
# but WITHOUT ANY WARRANTY; without even the implied warranty of
# MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE. See the
# GNU General Public License for more details.
#
# You should have received a copy of the GNU General Public License
# along with this program; if not, write to the Free Software
# Foundation, Inc., 51 Franklin Street, Fifth Floor, Boston, MA 02110-1301, USA        
#

#
# Configuration options:
#
PORT    = 8080                
WIDTH   = 0  # eg.: 640, 800, 1024, 0 for auto
HEIGHT  = 0  # eg.: 480, 600,  768, 0 for auto
WAIT    = 1  # sleep for 1 second to allow javascript renders
QUALITY = 80 # jpeg image quality 0-100 

__version__ = "1.1qt"

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
import signal
import logging
from PyQt4.QtCore import *
from PyQt4.QtGui import *
from PyQt4.QtWebKit import *
from PyQt4.QtNetwork import *

logging.basicConfig(filename='/dev/stdout',level=logging.WARN,)
logger = logging.getLogger('wrp');

# Class for Website-Rendering. Uses QWebPage, which
# requires a running QtGui to work.
class WebkitRenderer(QObject):
    def __init__(self,**kwargs):
        """Sets default values for the properties."""

        if not QApplication.instance():
            raise RuntimeError(self.__class__.__name__ + " requires a running QApplication instance")
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
        helper._window.resize( self.width, self.height )
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
        for key,value in parent.__dict__.items():
            setattr(self,key,value)

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
        self.connect(self._page, SIGNAL("loadFinished(bool)"), self._on_load_finished)
        self.connect(self._page, SIGNAL("loadStarted()"), self._on_load_started)
        self.connect(self._page.networkAccessManager(), SIGNAL("sslErrors(QNetworkReply *,const QList<QSslError>&)"), self._on_ssl_errors)
        self.connect(self._page.networkAccessManager(), SIGNAL("finished(QNetworkReply *)"), self._on_each_reply)

        # The way we will use this, it seems to be unesseccary to have Scrollbars enabled
        self._page.mainFrame().setScrollBarPolicy(Qt.Horizontal, Qt.ScrollBarAlwaysOff)
        self._page.mainFrame().setScrollBarPolicy(Qt.Vertical, Qt.ScrollBarAlwaysOff)
        self._page.settings().setUserStyleSheetUrl(QUrl("data:text/css,html,body{overflow-y:hidden !important;}"))

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
            image = QPixmap.grabWindow(self._window.winId())
        else:
            image = QPixmap.grabWidget(self._window)

        # Write URL map
        httpout.write("<!-- Web Rendering Proxy v%s by Antoni Sawicki -->\n<html>\n<body>\n<img src=\"http://%s\" alt=\"webrender\" usemap=\"#map\">\n<map name=\"map\">\n" % (__version__, IMG))
        frame = self._view.page().currentFrame()
        for x in frame.findAllElements('a'):
            value = x.attribute('href')
            xmin, ymin, xmax, ymax = x.geometry().getCoords() 
            httpout.write("<area shape=\"rect\" coords=\"%i,%i,%i,%i\" alt=\"%s\" href=\"%s\">\n" % (xmin, ymin, xmax, ymax, value, value))
        httpout.write("</map>\n</body>\n</html>\n")

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

    def _on_each_reply(self,reply):
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
        """This function is called whenever a JavaScript program running inside frame tries to prompt
        the user for input. The program may provide an optional message, msg, as well as a default value
        for the input in defaultValue.

        If the prompt was cancelled by the user the implementation should return false;
        otherwise the result should be written to result and true should be returned.
        If the prompt was not cancelled by the user, the implementation should return true and
        the result string must not be null.
        """
        if self.logger: self.logger.debug('Prompt: %s (%s)' % (message, default))
        return False

    def shouldInterruptJavaScript(self):
        """This function is called when a JavaScript program is running for a long period of time.
        If the user wanted to stop the JavaScript the implementation should return true; otherwise false.
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


# Request queue (URLs go in here)
REQ = Queue.Queue()
# Response queue (dummy response objects)
RESP = Queue.Queue()

#import pdb; pdb.set_trace()

class Proxy(SimpleHTTPServer.SimpleHTTPRequestHandler):
    def do_GET(self):
        req_url=self.path
        global httpout
        httpout=self.wfile
        self.send_response(200, 'OK')

        jpg_re = re.compile("http://webrender-[0-9]+\.jpg")
        ico_re = re.compile(".+\.ico")

        if (jpg_re.search(req_url)):
            img=req_url.split("/")
            print ">>> request for rendered jpg image... %s  [%d kb]" % (img[2], os.path.getsize(img[2])/1024)
            self.send_header('Content-type', 'image/jpeg')
            self.end_headers()  
            fimg = open(img[2])
            httpout.write(fimg.read())
            fimg.close()
            os.remove(img[2])
            
        elif (ico_re.search(req_url)):
            print ">>> request for .ico file - skipping"
            self.send_error(404, "ICO not supported")       
            self.end_headers()
          
        else:
            print ">>> request for url: " + req_url
            self.send_header('Content-type', 'text/html')
            self.end_headers()  

            global IMG
            IMG = "webrender-%s.jpg" % (random.randrange(0,1000))

            # To thread
            REQ.put(req_url)
            # Wait for completition
            RESP.get()

def run_proxy():
    httpd = SocketServer.TCPServer(('', PORT), Proxy)
    print "Web Rendering Proxy v%s serving port: %s" % (__version__, PORT)
    while 1:
        httpd.serve_forever()

def main():
    # Launch Proxy Thread
    threading.Thread(target=run_proxy).start()

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
                rurl = REQ.get()
                if rurl == "http://wrp.stop/":
                    print ">>> Terminate Request Received"
                    break

                # Initialize WebkitRenderer object
                renderer = WebkitRenderer()
                renderer.logger = logger
                renderer.width = WIDTH
                renderer.height = HEIGHT
                renderer.timeout = 60
                renderer.wait = WAIT
                renderer.grabWholeWindow = False

                image = renderer.render(rurl)
                qBuffer = QBuffer()
                image.save(qBuffer, 'jpg', QUALITY)

                output = open(IMG, 'w')
                output.write(qBuffer.buffer().data())
                output.close()

                del renderer
                print ">>> done: %s [%d kb]..." % (IMG, os.path.getsize(IMG)/1024)
                
                RESP.put('')

            QApplication.exit(0)
        except RuntimeError, e:
            logger.error("main: %s" % e)
            print >> sys.stderr, e
            QApplication.exit(1)

    # Initialize Qt-Application, but make this script
    # abortable via CTRL-C
    app = init_qtgui(display=None, style=None)
    signal.signal(signal.SIGINT, signal.SIG_DFL)

    QTimer.singleShot(0, __main_qt)
    sys.exit(app.exec_())

if __name__ == '__main__' : main()
