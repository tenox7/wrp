#!/usr/bin/env python

# webrender.py - recursively render web pages to a gif+imagemap of clickable links
# caveat: this script requires to be run as a regular user and cannot run as a daemon
# from apache cgi-bin, you can use python built in http server instead
# usage:
#   create cgi-bin directory, copy webrender.py to cgi-bin and chmod 755
#   python -m CGIHTTPServer 8000 
#   navigate web browser to http://x.x.x.x:8000/cgi-bin/webrender.py
# the webrender-xxx.gif images are created in the CWD of the http server


__version__ = "1.0"

# 
# This program is based on the software picidae.py 1.0 from http://www.picidae.net
# It was modified by Antoni Sawicki
# 
# This program is based on the software webkit2png 0.4 from Paul Hammond.
# It was extended by picidae.net
# 
# This program is free software; you can redistribute it and/or
# modify it under the terms of the GNU General Public License
# as published by the Free Software Foundation; either version 2
# of the License, or (at your option) any later version.
#
# This program is distributed in the hope that it will be useful,
# but WITHOUT ANY WARRANTY; without even the implied warranty of
# MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE.  See the
# GNU General Public License for more details.
#
# You should have received a copy of the GNU General Public License
# along with this program; if not, write to the Free Software
# Foundation, Inc., 59 Temple Place - Suite 330, Boston, MA  02111-1307, USA.
                
try:
  import sys
  import os
  import glob
  import random
  import Foundation
  import WebKit
  import AppKit
  import objc
  import string
  import urllib
  import socket
  import cgi
  import cgitb; cgitb.enable() # for trubleshooting
except ImportError:
  print "Cannot find pyobjc library files.  Are you sure it is installed?"
  sys.exit() 


from optparse import OptionParser


class AppDelegate (Foundation.NSObject):
    # what happens when the app starts up
    def applicationDidFinishLaunching_(self, aNotification):
        webview = aNotification.object().windows()[0].contentView()
        webview.frameLoadDelegate().getURL(webview)


class WebkitLoad (Foundation.NSObject, WebKit.protocols.WebFrameLoadDelegate):
    # what happens if something goes wrong while loading
    def webView_didFailLoadWithError_forFrame_(self,webview,error,frame):
        print " ... something went wrong 1: " + error.localizedDescription()
        self.getURL(webview)
    def webView_didFailProvisionalLoadWithError_forFrame_(self,webview,error,frame):
        print " ... something went wrong 2: " + error.localizedDescription()
        self.getURL(webview)

    def getURL(self,webview):
        if self.urls:
            if self.urls[0] == '-':
                url = sys.stdin.readline().rstrip()
                if not url: AppKit.NSApplication.sharedApplication().terminate_(None)
            else: 
                url = self.urls.pop(0)
        else:
            AppKit.NSApplication.sharedApplication().terminate_(None)

        self.resetWebview(webview)
        webview.mainFrame().loadRequest_(Foundation.NSURLRequest.requestWithURL_(Foundation.NSURL.URLWithString_(url)))
        if not webview.mainFrame().provisionalDataSource():
            print "<nosuccess  />"
            self.getURL(webview)
     
    def resetWebview(self,webview):
        rect = Foundation.NSMakeRect(0,0,1024,768)
        webview.window().setContentSize_((1024,768))
        webview.setFrame_(rect)
    
    def resizeWebview(self,view):
        view.window().display()
        view.window().setContentSize_(view.bounds().size)
        view.setFrame_(view.bounds())

    def captureView(self,view):
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

            self.resizeWebview(view)

            URL = frame.dataSource().initialRequest().URL().absoluteString()

            for fl in glob.glob("webrender-*.gif"):
                os.remove(fl)

            GIF = "webrender-%s.gif" % (random.randrange(0,1000))

            bitmapdata = self.captureView(view)  
            bitmapdata.representationUsingType_properties_(AppKit.NSGIFFileType,None).writeToFile_atomically_(GIF,objc.YES)

            myurl = "http://%s:%s%s" % (socket.gethostbyname(socket.gethostname()), os.getenv("SERVER_PORT"), os.getenv("SCRIPT_NAME"))

            print "Content-type: text/html\r\n\r\n"
            print "<!-- webrender.py by Antoni Sawicki -->"
            print "<html><head><title>Webrender - %s</title></head><body><table border=\"0\"><tr>" % (URL)
            print "<td><form action=\"%s\">" % (myurl)
            print "<input type=\"text\" name=\"url\" value=\"%s\" size=\"80\">" % (URL)
            print "<input type=\"submit\" value=\"go\">"
            print "</form></td><td>"
            print "<form action=\"%s\">" % (myurl)
            print "<input type=\"text\" name=\"search\" value=\"\" size=\"20\">"
            print "<input type=\"submit\" value=\"search\">"
            print "</form></td></tr></table>"
            print "<img src=\"../%s\" alt=\"webrender\" usemap=\"#map\" border=\"0\">" % (GIF)


            # Analyse HTML and get links
            print "<map name=\"map\">";
            
            domdocument = frame.DOMDocument()
            domnodelist = domdocument.getElementsByTagName_('A')
            i = 0
            while  i < domnodelist.length():
            	# linkvalue
            	value = domnodelist.item_(i).valueForKey_('href')
            	
            	# position-rect
            	myrect = domnodelist.item_(i).boundingBox()
            	
            	xmin = Foundation.NSMinX(myrect)
            	ymin = Foundation.NSMinY(myrect)
            	xmax = Foundation.NSMaxX(myrect)
            	ymax = Foundation.NSMaxY(myrect)
            	
            	# print Link
            	escval = string.replace( string.replace(value, "?", "TNXQUE"), "&", "TNXAMP" )
            	print "<area shape=\"rect\" coords=\"%i,%i,%i,%i\" alt=\"\" href=\"%s?url=%s\"></area>" % (xmin, ymin, xmax, ymax, myurl, escval)
            	i += 1
            
            print "</map>"
            print "</body></html>"
            self.getURL(webview)


def main():

    # obtain url from cgi input
    form = cgi.FieldStorage()
    rawurl = form.getfirst("url", "http://www.google.com")
    rawsearch = form.getfirst("search")
    if rawsearch:
        url = "http://www.google.com/search?q=%s" % (rawsearch)
    else:
        url = string.replace( string.replace(rawurl, "TNXAMP", "&"), "TNXQUE", "?")


    AppKit.NSApplicationLoad();	

    app = AppKit.NSApplication.sharedApplication()
    
    # create an app delegate
    delegate = AppDelegate.alloc().init()
    AppKit.NSApp().setDelegate_(delegate)
	
    # create a window
    rect = Foundation.NSMakeRect(-16000,-16000,100,100)
    win = AppKit.NSWindow.alloc()
    win.initWithContentRect_styleMask_backing_defer_ (rect, AppKit.NSBorderlessWindowMask, 2, 0)
	
    # create a webview object
    webview = WebKit.WebView.alloc()
    webview.initWithFrame_(rect)
    # turn off scrolling so the content is actually x wide and not x-15
    webview.mainFrame().frameView().setAllowsScrolling_(objc.NO)
    # add the webview to the window
    win.setContentView_(webview)
    
   
    # create a LoadDelegate
    loaddelegate = WebkitLoad.alloc().init()
    loaddelegate.options = [""]
    loaddelegate.urls = [url]
    webview.setFrameLoadDelegate_(loaddelegate)
   	
    app.run()    

if __name__ == '__main__' : main()

