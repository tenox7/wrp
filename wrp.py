#!/usr/bin/env python2.7

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

# claunia: Check how to use this in macOS
#logging.basicConfig(filename='/dev/stdout',level=logging.WARN,)
#logger = logging.getLogger('wrp');

# Request queue (URLs go in here)
REQ = Queue.Queue()
# Response queue (dummy response objects)
RESP = Queue.Queue()

#######################
### COMMON CODEPATH ###
#######################
class Proxy(SimpleHTTPServer.SimpleHTTPRequestHandler):
    def do_GET(self):
        req_url=self.path
        httpout=self.wfile
        
        gif_re = re.match("http://(wrp-\d+\.gif).*", req_url)
        map_re = re.match("http://(wrp-\d+\.map).*?(\d+),(\d+)", req_url)
        ico_re = re.match("http://.+\.ico", req_url)
        jpg_re = re.match("http://(wrp-\d+\.jpg).*", req_url)

        # Serve Rendered GIF
        if (gif_re):
            img=gif_re.group(1)
            print ">>> GIF file request... " + img
            self.send_response(200, 'OK')
            self.send_header('Content-type', 'image/gif')
            self.end_headers()  
            fimg=open(img)
            httpout.write(fimg.read())
            fimg.close()
            os.remove(img)
            
        elif (jpg_re):
            img=jpg_re.group(1)
            print ">>> request for rendered jpg image... %s  [%d kb]" % (img, os.path.getsize(img)/1024)
            self.send_response(200, 'OK')
            self.send_header('Content-type', 'image/jpeg')
            self.end_headers()  
            fimg = open(img)
            httpout.write(fimg.read())
            fimg.close()
            os.remove(img)

        # Process ISMAP Request
        elif (map_re):
            map=map_re.group(1)
            req_x=int(map_re.group(2))
            req_y=int(map_re.group(3))
            print ">>> ISMAP request... %s [%d,%d] " % (map, req_x, req_y)

            with open(map) as mapf:
                goto_url="none"
                for line in mapf.readlines(): 
                    if(re.match("(\S+)", line).group(1) == "default"):
                        default_url=re.match("\S+\s+(\S+)", line).group(1)
    
                    elif(re.match("(\S+)", line).group(1) == "rect"):
                        rect=re.match("(\S+)\s+(\S+)\s+(\d+),(\d+)\s+(\d+),(\d+)", line)
                        min_x=int(rect.group(3))
                        min_y=int(rect.group(4))
                        max_x=int(rect.group(5))
                        max_y=int(rect.group(6))
                        if( (req_x >= min_x) and (req_x <= max_x) and (req_y >= min_y) and (req_y <= max_y) ):
                            goto_url=rect.group(2)
                        
            mapf.close()
            
            if(goto_url == "none"):
                goto_url=default_url

            print(">>> ISMAP redirect: %s\n" % (goto_url))

            self.send_response(302, "Found")
            self.send_header("Location", goto_url)
            self.send_header("Content-type", "text/html")
            self.end_headers()
            httpout.write("<HTML><BODY><A HREF=\"%s\">%s</A></BODY></HTML>\n" % (goto_url, goto_url))
            
        # ICO files, WebKit crashes on these
        elif (ico_re):
            self.send_error(415, "ICO not supported")       
            self.end_headers()
          
        # Process a web page request and generate image
        else:
            print ">>> URL request... " + req_url
            self.send_response(200, 'OK')
            self.send_header('Content-type', 'text/html')
            self.end_headers()  

            rnd = random.randrange(0,1000)

            if sys.platform == "linux" or sys.platform == "linux2":
                import wrp_qt

                "wrp-%s.jpg" % (rnd)
                "wrp-%s.map" % (rnd)

                # To thread
                wrp_qt.REQ.put((httpout, req_url, "wrp-%s.jpg" % (rnd), "wrp-%s.map" % (rnd)))
                # Wait for completition
                wrp_qt.RESP.get()
            elif sys.platform == "darwin":
                import wrp_cocoa

                "wrp-%s.gif" % (rnd)
                "wrp-%s.map" % (rnd)

                # To WebKit Thread
                wrp_cocoa.REQ.put((httpout, req_url, "wrp-%s.gif" % (rnd), "wrp-%s.map" % (rnd)))
                # Wait for completition
                wrp_cocoa.RESP.get()

def run_proxy():
    httpd = SocketServer.TCPServer(('', PORT), Proxy)
    print "Web Rendering Proxy v%s serving at port: %s" % (__version__, PORT)
    while 1:
        httpd.serve_forever()

def main():
    # Launch Proxy Thread
    threading.Thread(target=run_proxy).start()

    if sys.platform == "linux" or sys.platform == "linux2":
        import signal
        import PyQt4.QtCore
        import wrp_qt
        # Initialize Qt-Application, but make this script
        # abortable via CTRL-C
        app = wrp_qt.init_qtgui(display=None, style=None)
        signal.signal(signal.SIGINT, signal.SIG_DFL)

        PyQt4.QtCore.QTimer.singleShot(0, wrp_qt.__main_qt)
        sys.exit(app.exec_())
    elif sys.platform == "darwin":
        import wrp_cocoa
        wrp_cocoa.main()
    else:
        sys.exit("Unsupported platform: %s. Exiting." % sys.platform)

if __name__ == '__main__' : main()
