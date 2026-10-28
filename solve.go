package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"net/url"
	"strings"
	"time"
	"unicode/utf8"
)

var deniedTitles = []string{"Access denied", "Attention Required! | Cloudflare"}
var deniedSelectors = []string{"div.cf-error-title span.cf-code-label span", "#cf-error-details div.cf-error-overview h1"}
var challengeTitles = []string{"Just a moment...", "DDoS-Guard"}
var challengeSelectors = []string{"#cf-challenge-running", ".ray_id", ".attack-box", "#cf-please-wait", "#challenge-spinner", "#trk_jschal_js", "#turnstile-wrapper", ".lds-ring", "td.info #js_info", "div.vc div.text-box h2"}

const elementKey = "element-6066-11e4-a52e-4f735466cecf"
const turnstileSelector = "input[name='cf-turnstile-response']"

var errChallengeWait = errors.New("challenge is still present")

type pageBrowser interface {
	Call(context.Context, string, string, any) (any, error)
	Execute(context.Context, string, ...any) (any, error)
	Navigate(context.Context, string) error
	CDP(context.Context, string, any) (any, error)
}

func Solve(ctx context.Context, browser *Browser, req Request, cfg Config) (map[string]any, string, error) {
	return solvePage(ctx, browser, req, cfg)
}

func solvePage(ctx context.Context, browser pageBrowser, req Request, cfg Config) (map[string]any, string, error) {
	logger := cfg.Logger
	if logger == nil {
		logger = slog.Default()
	}
	disableMedia := cfg.DisableMedia
	if req["disableMedia"] != nil {
		disableMedia = truthy(req["disableMedia"])
	}
	if disableMedia {
		blockMedia(ctx, browser, logger)
	}

	navigate := func() error {
		if textValue(req["cmd"]) == "request.post" {
			return browser.Navigate(ctx, postFormURL(req))
		}
		return browser.Navigate(ctx, textValue(req["url"]))
	}
	if err := navigate(); err != nil {
		return nil, "", err
	}
	var turnstileToken any
	if req["tabs_till_verify"] != nil && textValue(req["cmd"]) != "request.post" {
		found, err := hasElements(ctx, browser, turnstileSelector)
		if err != nil {
			return nil, "", err
		}
		if found {
			tabs, err := numericValue(req["tabs_till_verify"])
			if err != nil {
				return nil, "", err
			}
			token, err := solveTurnstile(ctx, browser, int(tabs), logger)
			if err != nil {
				return nil, "", err
			}
			turnstileToken = token
		}
	}

	if truthy(req["cookies"]) {
		cookies, ok := req["cookies"].([]any)
		if !ok {
			return nil, "", errors.New("cookies must be a list")
		}
		for _, value := range cookies {
			cookie, ok := value.(map[string]any)
			if !ok {
				return nil, "", errors.New("cookie must be an object")
			}
			name, ok := cookie["name"]
			if !ok {
				return nil, "", errors.New("'name'")
			}
			if _, err := browser.Call(ctx, "DELETE", "/cookie/"+url.PathEscape(textValue(name)), nil); err != nil {
				return nil, "", err
			}
			if _, err := browser.Call(ctx, "POST", "/cookie", map[string]any{"cookie": cookie}); err != nil {
				return nil, "", err
			}
		}
		if err := navigate(); err != nil {
			return nil, "", err
		}
	}
	if cfg.LogHTML && logger.Enabled(ctx, slog.LevelDebug) {
		source, err := browser.Call(ctx, "GET", "/source", nil)
		if err != nil {
			return nil, "", err
		}
		logger.Debug("Response HTML", "html", source)
	}
	htmlElement, err := findElement(ctx, browser, "css selector", "html")
	if err != nil {
		return nil, "", err
	}
	titleValue, err := browser.Call(ctx, "GET", "/title", nil)
	if err != nil {
		return nil, "", err
	}
	title := textValue(titleValue)
	for _, denied := range deniedTitles {
		if strings.HasPrefix(title, denied) {
			return nil, "", accessDeniedError()
		}
	}
	denied, err := hasElements(ctx, browser, strings.Join(deniedSelectors, ", "))
	if err != nil {
		return nil, "", err
	}
	if denied {
		return nil, "", accessDeniedError()
	}
	challenge := false
	for _, candidate := range challengeTitles {
		if strings.EqualFold(title, candidate) {
			challenge = true
			break
		}
	}
	if !challenge {
		challenge, err = hasElements(ctx, browser, strings.Join(challengeSelectors, ", "))
		if err != nil {
			return nil, "", err
		}
	}
	message := "Challenge not detected!"
	if challenge {
		logger.Info("Challenge detected", "title", title)
		for {
			err = waitForChallengeToDisappear(ctx, browser, cfg.BrowserWaitTimeout)
			if err == nil {
				break
			}
			if !errors.Is(err, errChallengeWait) {
				return nil, "", err
			}
			logger.Debug("Challenge remains; attempting verification")
			if err := clickVerify(ctx, browser, 1, logger); err != nil {
				return nil, "", err
			}
			htmlElement, err = findElement(ctx, browser, "css selector", "html")
			if err != nil {
				return nil, "", err
			}
		}
		// The original implementation waits for the old document to become stale
		// after challenge disappearance, and ignores a redirect wait timeout.
		_ = waitForPage(ctx, cfg.BrowserWaitTimeout, func() (bool, error) {
			_, err := browser.Call(ctx, "GET", "/element/"+url.PathEscape(elementID(htmlElement))+"/enabled", nil)
			if err != nil && strings.Contains(strings.ToLower(err.Error()), "stale element") {
				return true, nil
			}
			return false, err
		})
		if err := ctx.Err(); err != nil {
			return nil, "", err
		}
		message = "Challenge solved!"
	}
	logger.Info(message)
	finalURL, err := browser.Call(ctx, "GET", "/url", nil)
	if err != nil {
		return nil, "", err
	}
	userAgent := cfg.UserAgent
	if userAgent == "" {
		value, err := browser.Execute(ctx, "return navigator.userAgent")
		if err != nil {
			return nil, "", err
		}
		userAgent = headlessWord.ReplaceAllString(textValue(value), "")
	}
	solution := map[string]any{"url": finalURL, "status": 200, "userAgent": userAgent, "turnstile_token": turnstileToken}
	if !truthy(req["returnOnlyCookies"]) {
		solution["headers"] = map[string]any{}
		if truthy(req["waitInSeconds"]) {
			seconds, err := numericValue(req["waitInSeconds"])
			if err != nil {
				return nil, "", err
			}
			if seconds > 0 {
				if err := sleepPage(ctx, time.Duration(seconds*float64(time.Second))); err != nil {
					return nil, "", err
				}
			}
		}
		solution["response"], err = browser.Call(ctx, "GET", "/source", nil)
		if err != nil {
			return nil, "", err
		}
	}
	if truthy(req["returnScreenshot"]) {
		solution["screenshot"], err = browser.Call(ctx, "GET", "/screenshot", nil)
		if err != nil {
			return nil, "", err
		}
	}
	solution["cookies"], err = browser.Call(ctx, "GET", "/cookie", nil)
	if err != nil {
		return nil, "", err
	}
	return solution, message, nil
}

func accessDeniedError() error {
	return errors.New("Cloudflare has blocked this request. Probably your IP is banned for this site, check in your web browser.")
}

func findElement(ctx context.Context, browser pageBrowser, using, selector string) (any, error) {
	return browser.Call(ctx, "POST", "/element", map[string]any{"using": using, "value": selector})
}

func elementID(value any) string {
	if element, ok := value.(map[string]any); ok {
		if id, ok := element[elementKey].(string); ok {
			return id
		}
		if id, ok := element["ELEMENT"].(string); ok {
			return id
		}
	}
	return ""
}

func hasElements(ctx context.Context, browser pageBrowser, selector string) (bool, error) {
	value, err := browser.Call(ctx, "POST", "/elements", map[string]any{"using": "css selector", "value": selector})
	if err != nil {
		return false, err
	}
	elements, ok := value.([]any)
	if !ok {
		return false, fmt.Errorf("invalid elements response: %T", value)
	}
	return len(elements) > 0, nil
}

func waitForChallengeToDisappear(ctx context.Context, browser pageBrowser, timeout time.Duration) error {
	for _, title := range challengeTitles {
		if err := waitForPage(ctx, timeout, func() (bool, error) {
			value, err := browser.Call(ctx, "GET", "/title", nil)
			return textValue(value) != title, err
		}); err != nil {
			return err
		}
	}
	for _, selector := range challengeSelectors {
		if err := waitForPage(ctx, timeout, func() (bool, error) {
			present, err := hasElements(ctx, browser, selector)
			return !present, err
		}); err != nil {
			return err
		}
	}
	return nil
}

func waitForPage(ctx context.Context, timeout time.Duration, ready func() (bool, error)) error {
	deadline := time.Now().Add(timeout)
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		done, err := ready()
		if err != nil {
			return err
		}
		if done {
			return nil
		}
		if !time.Now().Before(deadline) {
			return errChallengeWait
		}
		if err := sleepPage(ctx, 500*time.Millisecond); err != nil {
			return err
		}
	}
}

func sleepPage(ctx context.Context, delay time.Duration) error {
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

func clickVerify(ctx context.Context, browser pageBrowser, tabs int, logger *slog.Logger) error {
	// Match Selenium ActionChains: keyboard and pointer actions share ticks.
	keyActions := []any{actionPause(5.1 + rand.Float64()*0.4)}
	for i := 0; i < tabs; i++ {
		keyActions = append(keyActions, map[string]any{"type": "keyDown", "value": "\ue004"}, map[string]any{"type": "keyUp", "value": "\ue004"}, actionPause(0.12+rand.Float64()*0.08))
	}
	keyActions = append(keyActions, actionPause(1.1+rand.Float64()*0.4), map[string]any{"type": "keyDown", "value": " "}, map[string]any{"type": "keyUp", "value": " "})
	pointerActions := make([]any, len(keyActions))
	for i := range pointerActions {
		pointerActions[i] = map[string]any{"type": "pause", "duration": 0}
	}
	_, clickErr := browser.Call(ctx, "POST", "/actions", map[string]any{"actions": []any{
		map[string]any{"type": "pointer", "id": "mouse", "parameters": map[string]any{"pointerType": "mouse"}, "actions": pointerActions},
		map[string]any{"type": "key", "id": "key", "actions": keyActions},
	}})
	if clickErr != nil {
		logger.Debug("Verification checkbox action failed", "error", clickErr)
	}
	if _, err := browser.Call(ctx, "POST", "/frame", map[string]any{"id": nil}); err != nil {
		return err
	}
	button, err := findElement(ctx, browser, "xpath", "//input[@type='button' and @value='Verify you are human']")
	if err == nil {
		_, err = browser.Call(ctx, "POST", "/actions", map[string]any{"actions": []any{
			map[string]any{"type": "pointer", "id": "mouse", "parameters": map[string]any{"pointerType": "mouse"}, "actions": []any{
				map[string]any{"type": "pointerMove", "duration": 250, "origin": button, "x": 5, "y": 7},
				map[string]any{"type": "pointerDown", "duration": 0, "button": 0},
				map[string]any{"type": "pointerUp", "duration": 0, "button": 0},
			}},
			map[string]any{"type": "key", "id": "key", "actions": []any{actionPause(0), actionPause(0), actionPause(0)}},
		}})
	}
	if err != nil {
		logger.Debug("Verification button not available", "error", err)
	}
	return sleepPage(ctx, 2*time.Second)
}

func actionPause(seconds float64) map[string]any {
	return map[string]any{"type": "pause", "duration": int(seconds * 1000)}
}

func solveTurnstile(ctx context.Context, browser pageBrowser, tabs int, logger *slog.Logger) (string, error) {
	input, err := findElement(ctx, browser, "css selector", turnstileSelector)
	if err != nil {
		return "", err
	}
	readToken := func() (string, error) {
		value, err := browser.Execute(ctx, "return arguments[0].value || arguments[0].getAttribute('value')", input)
		if value == nil {
			return "", err
		}
		return textValue(value), err
	}
	original, err := readToken()
	if err != nil {
		return "", err
	}
	for {
		if err := clickVerify(ctx, browser, tabs, logger); err != nil {
			return "", err
		}
		token, err := readToken()
		if err != nil {
			return "", err
		}
		if token != "" && token != original {
			return token, nil
		}
		_, err = browser.Execute(ctx, `let old = document.getElementById('__focus_helper');
if (old) old.remove();
let el = document.createElement('button');
el.id = '__focus_helper';
el.style.position = 'fixed';
el.style.top = '0';
el.style.left = '0';
el.style.opacity = '0.01';
el.style.pointerEvents = 'none';
document.body.prepend(el);
el.focus();`)
		if err != nil {
			return "", err
		}
		if err := sleepPage(ctx, time.Second); err != nil {
			return "", err
		}
	}
}

func blockMedia(ctx context.Context, browser pageBrowser, logger *slog.Logger) {
	extensions := []string{"png", "jpg", "jpeg", "gif", "webp", "bmp", "svg", "ico", "tiff", "tif", "jpe", "apng", "avif", "heic", "heif", "css", "woff", "woff2", "ttf", "otf", "eot"}
	patterns := make([]string, 0, 2*len(extensions))
	for _, extension := range extensions {
		patterns = append(patterns, "*."+extension, "*."+strings.ToUpper(extension))
	}
	_, err := browser.CDP(ctx, "Network.enable", map[string]any{})
	if err == nil {
		_, err = browser.CDP(ctx, "Network.setBlockedURLs", map[string]any{"urls": patterns})
	}
	if err != nil {
		logger.Debug("Resource blocking unavailable", "error", err)
	}
}

func postFormURL(req Request) string {
	query := strings.TrimPrefix(textValue(req["postData"]), "?")
	var form strings.Builder
	fmt.Fprintf(&form, `<form id="hackForm" action="%s" method="POST">`, textValue(req["url"]))
	for _, pair := range strings.Split(query, "&") {
		parts := strings.SplitN(pair, "=", 2)
		name := unquotePost(parts[0])
		if name == "submit" {
			continue
		}
		value := ""
		if len(parts) > 1 {
			value = unquotePost(parts[1])
		}
		value = strings.ReplaceAll(value, `"`, "&quot;")
		fmt.Fprintf(&form, `<input type="text" name="%s" value="%s"><br>`, quotePost(name), quotePost(value))
	}
	form.WriteString("</form>")
	return "data:text/html;charset=utf-8," + fmt.Sprintf(`
        <!DOCTYPE html>
        <html>
        <body>
            %s
            <script>document.getElementById('hackForm').submit();</script>
        </body>
        </html>`, form.String())
}

func unquotePost(value string) string {
	// Python urllib.parse.unquote leaves malformed percent escapes untouched and
	// replaces invalid UTF-8, whereas Go's PathUnescape rejects malformed escapes.
	var result strings.Builder
	for i := 0; i < len(value); i++ {
		if value[i] == '%' && i+2 < len(value) {
			if decoded, err := url.PathUnescape(value[i : i+3]); err == nil {
				result.WriteString(decoded)
				i += 2
				continue
			}
		}
		result.WriteByte(value[i])
	}
	raw := result.String()
	if utf8.ValidString(raw) {
		return raw
	}
	var valid strings.Builder
	for len(raw) > 0 {
		r, n := utf8.DecodeRuneInString(raw)
		if r == utf8.RuneError && n == 1 {
			n = invalidUTF8PrefixLength(raw)
		}
		valid.WriteRune(r)
		raw = raw[n:]
	}
	return valid.String()
}

func invalidUTF8PrefixLength(value string) int {
	first := value[0]
	length := 0
	switch {
	case first >= 0xc2 && first <= 0xdf:
		length = 2
	case first >= 0xe0 && first <= 0xef:
		length = 3
	case first >= 0xf0 && first <= 0xf4:
		length = 4
	default:
		return 1
	}
	consumed := 1
	for consumed < length && consumed < len(value) {
		next := value[consumed]
		if next < 0x80 || next > 0xbf {
			break
		}
		if consumed == 1 && (first == 0xe0 && next < 0xa0 || first == 0xed && next > 0x9f || first == 0xf0 && next < 0x90 || first == 0xf4 && next > 0x8f) {
			break
		}
		consumed++
	}
	return consumed
}

func quotePost(value string) string {
	const hex = "0123456789ABCDEF"
	var result strings.Builder
	for i := 0; i < len(value); i++ {
		c := value[i]
		if c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || strings.ContainsRune("_.-~/", rune(c)) {
			result.WriteByte(c)
		} else {
			result.WriteByte('%')
			result.WriteByte(hex[c>>4])
			result.WriteByte(hex[c&15])
		}
	}
	return result.String()
}
