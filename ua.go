// WRP user agent and client hints
//
// Bot checks compare the User-Agent with the client hints (the Sec-CH-UA*
// headers and navigator.userAgentData). Chrome's --user-agent switch changes
// only the string and sends the hints empty, which no real browser does:
// Google search showed a captcha for it. Without the switch, headless puts
// HeadlessChrome in the string. So every tab gets both over CDP instead.
package main

import (
	"context"
	"encoding/base64"
	"log"
	"regexp"
	"strings"

	"github.com/chromedp/cdproto/cdp"
	"github.com/chromedp/cdproto/emulation"
	"github.com/chromedp/cdproto/fetch"
	"github.com/chromedp/cdproto/runtime"
	"github.com/chromedp/chromedp"
)

// Set on every tab, nil: the browser's own agent
var uaOvr *emulation.SetUserAgentOverrideParams

// The browser's hints can only be read on a secure page. This one is served
// by CDP itself and never goes to the network.
const uaProbe = "https://wrp.invalid/"

// uaInit works out the agent and its hints on the first tab, before that
// tab loads anything: the browser's own agent without "Headless", or the -ua
// one with the browser's hints made to agree with it
func uaInit(ctx context.Context) {
	var ua string
	var md emulation.UserAgentMetadata
	lctx, lcncl := context.WithCancel(ctx)
	defer lcncl()
	chromedp.ListenTarget(lctx, func(ev interface{}) {
		if e, ok := ev.(*fetch.EventRequestPaused); ok {
			go fetch.FulfillRequest(e.RequestID, 200).
				WithResponseHeaders([]*fetch.HeaderEntry{{Name: "Content-Type", Value: "text/html"}}).
				WithBody(base64.StdEncoding.EncodeToString([]byte("<html></html>"))).
				Do(cdp.WithExecutor(ctx, chromedp.FromContext(ctx).Target))
		}
	})
	err := vncRun(ctx, vncTabTO,
		fetch.Enable().WithPatterns([]*fetch.RequestPattern{{URLPattern: uaProbe + "*"}}),
		chromedp.Navigate(uaProbe),
		chromedp.Evaluate(`navigator.userAgent`, &ua),
		chromedp.Evaluate(`navigator.userAgentData.getHighEntropyValues(["architecture",
			"bitness", "formFactors", "fullVersionList", "model", "platformVersion", "wow64"])`, &md,
			func(p *runtime.EvaluateParams) *runtime.EvaluateParams { return p.WithAwaitPromise(true) }),
		fetch.Disable(),
		chromedp.Navigate("about:blank"),
	)
	if err != nil {
		vncRun(ctx, vncTabTO, fetch.Disable(), chromedp.Navigate("about:blank"))
		log.Printf("Unable to read the browser's client hints: %v", err)
	}
	for _, l := range [][]*emulation.UserAgentBrandVersion{md.Brands, md.FullVersionList} {
		for _, b := range l {
			if b.Brand == "HeadlessChrome" || b.Brand == "Chrome" { // headless-shell
				b.Brand = "Google Chrome"
			}
		}
	}
	if *userAgent == "jnrbsn" {
		*userAgent = fetchJnrbsnUserAgent()
	}
	switch {
	case *userAgent != "" && err == nil:
		uaOvr = uaFit(*userAgent, &md)
	case *userAgent != "":
		uaOvr = emulation.SetUserAgentOverride(*userAgent) // with empty hints
	case err == nil:
		// Chrome/159.0.0.0 like a real Chrome: headless-shell gives the full version
		ua = regexp.MustCompile(`(Headless)?Chrome/(\d+)[\d.]*`).ReplaceAllString(ua, "Chrome/${2}.0.0.0")
		uaOvr = emulation.SetUserAgentOverride(ua).WithUserAgentMetadata(&md)
	default:
		return // the browser's own
	}
	var ch []string
	if m := uaOvr.UserAgentMetadata; m != nil {
		for _, b := range m.Brands {
			ch = append(ch, b.Brand+" "+b.Version)
		}
		ch = append(ch, m.Platform)
	}
	log.Printf("User agent: %s, client hints: %s", uaOvr.UserAgent, strings.Join(ch, ", "))
}

// uaFit makes the browser's hints agree with a -ua agent: its Chrome version
// and platform. The agent of another browser (no Chrome/) gets no hints.
func uaFit(ua string, md *emulation.UserAgentMetadata) *emulation.SetUserAgentOverrideParams {
	o := emulation.SetUserAgentOverride(ua)
	v := regexp.MustCompile(`Chrome/((\d+)[\d.]*)`).FindStringSubmatch(ua)
	if v == nil {
		return o
	}
	for _, b := range md.Brands {
		if !strings.Contains(b.Brand, "Brand") { // not the GREASE one
			b.Version = v[2]
		}
	}
	for _, b := range md.FullVersionList {
		if !strings.Contains(b.Brand, "Brand") {
			b.Version = v[1]
		}
	}
	for _, p := range []struct{ tok, ch, nav string }{
		{"Windows", "Windows", "Win32"},
		{"Android", "Android", "Linux armv8l"},
		{"CrOS", "Chrome OS", "Linux x86_64"},
		{"Macintosh", "macOS", "MacIntel"},
		{"Linux", "Linux", "Linux x86_64"},
	} {
		if !strings.Contains(ua, p.tok) {
			continue
		}
		if p.ch != md.Platform {
			md.Platform, md.PlatformVersion, md.Model = p.ch, "", ""
			o = o.WithPlatform(p.nav)
		}
		break
	}
	md.Mobile = strings.Contains(ua, "Mobile")
	return o.WithUserAgentMetadata(md)
}

// uaApply sets the agent on the tab behind ctx, for the pages it loads next
func uaApply(ctx context.Context) {
	if ctx == nil || uaOvr == nil {
		return
	}
	if err := vncRun(ctx, vncTabTO, uaOvr); err != nil {
		log.Printf("user agent not set on this tab: %v", err)
	}
}
