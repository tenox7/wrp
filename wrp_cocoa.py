#!/usr/bin/env python

# wrp.py - Web Rendering Proxy
# A HTTP proxy service that renders the requested URL in to a GIF image associated
# with an imagemap of clickable links. This is an adaptation of previous works by
# picidae.net and Paul Hammond.

__version__ = "1.3"

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
ISMAP  = "true"

import re
import random
import Foundation
import WebKit
import AppKit
import objc
import os
import io
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

# Handle map dictionary (in memory file names go here)
Handle = {}

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
        req = REQ.get()
        WebkitLoad.httpout = req[0]
        WebkitLoad.req_url = req[1]
        WebkitLoad.req_gif = req[2]
        WebkitLoad.req_map = req[3]
            
        if (WebkitLoad.req_url == "http://wrp.stop/"):
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
            bitmapdata.representationUsingType_properties_(AppKit.NSGIFFileType,None).writeToFile_atomically_(WebkitLoad.req_gif,objc.YES)

            # url of the rendered page
            web_url = frame.dataSource().initialRequest().URL().absoluteString()

            httpout = WebkitLoad.httpout

            httpout.write("<!-- Web Rendering Proxy v%s by Antoni Sawicki -->\n" % (__version__))
            httpout.write("<!-- Request for [%s] frame [%s] -->\n" % (WebkitLoad.req_url, web_url))
            httpout.write("<HTML><HEAD><TITLE>WRP%s:%s</TITLE></HEAD>\n<BODY>\n" % (__version__,web_url))
            if (ISMAP == "true"):
                httpout.write("<A HREF=\"http://%s\"><IMG SRC=\"http://%s\" ALT=\"wrp-render\" ISMAP>\n</A>\n" % (WebkitLoad.req_map, WebkitLoad.req_gif))
                mapfile = open(WebkitLoad.req_map, "w+")
                mapfile.write("default %s\n" % (web_url))
            else:
                httpout.write("<IMG SRC=\"http://%s\" ALT=\"wrp-render\" USEMAP=\"#map\">\n<MAP NAME=\"map\">\n" % (WebkitLoad.req_gif))
            
            domdocument = frame.DOMDocument()
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
                
                if (ISMAP == "true"):
                    mapfile.write("rect %s %i,%i %i,%i\n" % (turl, xmin, ymin, xmax, ymax))
                else:
                    httpout.write("<AREA SHAPE=\"RECT\" COORDS=\"%i,%i,%i,%i\" ALT=\"%s\" HREF=\"%s\">\n" % (xmin, ymin, xmax, ymax, turl, turl))
                
                i += 1
            
            if (ISMAP != "true"):
                httpout.write("</MAP>\n")
                
            httpout.write("</BODY>\n</HTML>\n")
            
            if (ISMAP == "true"):
                mapfile.close()

            # Return to Proxy thread and Loop...
            RESP.put('')
            self.getURL(webview)

def main():
    # Launch NS Application
    AppKit.NSApplicationLoad(); 
    app = AppKit.NSApplication.sharedApplication()
    delegate = AppDelegate.alloc().init()
    AppKit.NSApp().setDelegate_(delegate)
    AppKit.NSBundle.mainBundle().infoDictionary()['NSAppTransportSecurity'] = dict(NSAllowsArbitraryLoads = True)
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
