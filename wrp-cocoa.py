#!/usr/bin/env python

# wrp.py - Web Rendering Proxy
# A HTTP proxy service that renders the requested URL in to a GIF image associated
# with an imagemap of clickable links. This is an adaptation of previous works by
# picidae.net and Paul Hammond.

__version__ = "1.1"

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

import re
import random
import Foundation
import WebKit
import AppKit
import objc
import os
import time
import string
import urllib
import socket
import SocketServer
import SimpleHTTPServer
import threading
import Queue

# Request queue (URLs go in here)
REQ = Queue.Queue()
# Response queue (dummy response objects)
RESP = Queue.Queue()

#import pdb; pdb.set_trace()

class AppDelegate (Foundation.NSObject):
    # what happens when the app starts up
    def applicationDidFinishLaunching_(self, aNotification):
        webview = aNotification.object().windows()[0].contentView()
        webview.frameLoadDelegate().getURL(webview)

class WebkitLoad (Foundation.NSObject, WebKit.protocols.WebFrameLoadDelegate):
    # what happens if something goes wrong while loading
    def webView_didFailLoadWithError_forFrame_(self,webview,error,frame):
        if error.code() == Foundation.NSURLErrorCancelled:
            return
        print " ... something went wrong 1: " + error.localizedDescription()
        AppKit.NSApplication.sharedApplication().terminate_(None)

    def webView_didFailProvisionalLoadWithError_forFrame_(self,webview,error,frame):
        if error.code() == Foundation.NSURLErrorCancelled:
            return
        print " ... something went wrong 2: " + error.localizedDescription()
        AppKit.NSApplication.sharedApplication().terminate_(None)

    def getURL(self,webview):
        rurl = REQ.get()
            
        if (rurl == "http://wrp.stop/"):
            print ">>> Terminate Request Received"
            AppKit.NSApplication.sharedApplication().terminate_(None)

        nsurl = Foundation.NSURL.URLWithString_(rurl)
        if not (nsurl and nsurl.scheme()):
                nsurl = Foundation.NSURL.alloc().initFileURLWithPath_(url)
        nsurl = nsurl.absoluteURL()

        Foundation.NSURLRequest.setAllowsAnyHTTPSCertificate_forHost_(objc.YES, nsurl.host())

        self.resetWebview(webview)
        webview.mainFrame().loadRequest_(Foundation.NSURLRequest.requestWithURL_(nsurl))
        if not webview.mainFrame().provisionalDataSource():
            print " ... not a proper url?"
            RESP.put('')
            self.getURL(webview)
     
    def resetWebview(self,webview):
        rect = Foundation.NSMakeRect(0,0,WIDTH,HEIGHT)
        webview.window().setContentSize_((WIDTH,HEIGHT))
        webview.setFrame_(rect)
    
    def captureView(self,view):
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
    def webView_didFinishLoadForFrame_(self,webview,frame):
        # don't care about subframes
        if (frame == webview.mainFrame()):
            view = frame.frameView().documentView()

            bitmapdata = self.captureView(view)  
            bitmapdata.representationUsingType_properties_(AppKit.NSGIFFileType,None).writeToFile_atomically_(GIF,objc.YES)

            httpout.write("<!-- Web Rendering Proxy v%s by Antoni Sawicki -->\n<html>\n<body>\n<img src=\"http://%s\" alt=\"webrender\" usemap=\"#map\">\n<map name=\"map\">\n" % (__version__, GIF))
            
            domdocument = frame.DOMDocument()
            domnodelist = domdocument.getElementsByTagName_('A')
            i = 0
            while  i < domnodelist.length():
                value = domnodelist.item_(i).valueForKey_('href')
                myrect = domnodelist.item_(i).boundingBox()
                
                xmin = Foundation.NSMinX(myrect)
                ymin = Foundation.NSMinY(myrect)
                xmax = Foundation.NSMaxX(myrect)
                ymax = Foundation.NSMaxY(myrect)
                
                httpout.write("<area shape=\"rect\" coords=\"%i,%i,%i,%i\" alt=\"%s\" href=\"%s\">\n" % (xmin, ymin, xmax, ymax, value, value))
                i += 1
            
            httpout.write("</map>\n</body>\n</html>\n")
            
            RESP.put('')
            self.getURL(webview)

class Proxy(SimpleHTTPServer.SimpleHTTPRequestHandler):
    def do_GET(self):
        req_url=self.path
        global httpout
        httpout=self.wfile
        self.send_response(200, 'OK')

        gif_re = re.compile("http://webrender-[0-9]+\.gif")
        ico_re = re.compile(".+\.ico")

        if (gif_re.search(req_url)):
            img=req_url.split("/")
            print ">>> request for rendered gif image... %s" % (img[2])
            self.send_header('Content-type', 'image/gif')
            self.end_headers()  
            fimg = open(img[2])
            httpout.write(fimg.read())
            fimg.close()
            os.remove(img[2])
            
        elif (ico_re.search(req_url)):
            #print ">>> request for .ico file - skipping"
            self.send_error(404, "ICO not supported")       
            self.end_headers()
          
        else:
            print ">>> request for url: " + req_url
            self.send_header('Content-type', 'text/html')
            self.end_headers()  

            global GIF
            GIF = "webrender-%s.gif" % (random.randrange(0,1000))

            # To thread
            REQ.put(req_url)
            # Wait for completition
            RESP.get()

def run_proxy():
    httpd = SocketServer.TCPServer(('', PORT), Proxy)
    print "Web Rendering Proxy v%s serving at port: %s" % (__version__, PORT)
    while 1:
        httpd.serve_forever()

def main():
    # Launch Proxy Thread
    threading.Thread(target=run_proxy).start()

    # Launch NS Application
    AppKit.NSApplicationLoad(); 
    app = AppKit.NSApplication.sharedApplication()
    delegate = AppDelegate.alloc().init()
    AppKit.NSApp().setDelegate_(delegate)
    rect = Foundation.NSMakeRect(-16000,-16000,100,100)
    win = AppKit.NSWindow.alloc()
    win.initWithContentRect_styleMask_backing_defer_ (rect, AppKit.NSBorderlessWindowMask, 2, 0)
    webview = WebKit.WebView.alloc()
    webview.initWithFrame_(rect)
    webview.mainFrame().frameView().setAllowsScrolling_(objc.NO)
    webkit_version = Foundation.NSBundle.bundleForClass_(WebKit.WebView).objectForInfoDictionaryKey_(WebKit.kCFBundleVersionKey)[1:]
    webview.setApplicationNameForUserAgent_("Like-Version/6.0 Safari/%s wrp/%s" % (webkit_version, __version__))
    win.setContentView_(webview)
    loaddelegate = WebkitLoad.alloc().init()
    loaddelegate.options = [""]
    webview.setFrameLoadDelegate_(loaddelegate)
    app.run()  

if __name__ == '__main__' : main()

