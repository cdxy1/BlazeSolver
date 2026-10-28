package main

import (
	"archive/zip"
	"bytes"
	"context"
	"crypto/sha256"
	"debug/macho"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"
)

type Browser struct {
	Endpoint               string
	SessionID              string
	Client                 *http.Client
	headless               bool
	chrome, driver         *exec.Cmd
	chromeDone, driverDone chan error
	profile, extension     string
	closeOnce              sync.Once
}

func newBrowserClient(endpoint, sessionID string) *Browser {
	return &Browser{Endpoint: strings.TrimRight(endpoint, "/"), SessionID: sessionID,
		Client: &http.Client{Transport: &http.Transport{Proxy: nil, MaxIdleConnsPerHost: 2}}}
}

func LaunchBrowser(ctx context.Context, cfg Config, proxy map[string]any, userAgent string) (_ *Browser, err error) {
	startup, cancel := context.WithTimeout(ctx, 90*time.Second)
	defer cancel()
	chromePath, err := findChrome(cfg.ChromePath)
	if err != nil {
		return nil, err
	}
	driverPath, err := prepareDriver(startup, cfg, chromePath)
	if err != nil {
		return nil, err
	}
	debugPort, err := freePort()
	if err != nil {
		return nil, err
	}
	driverPort, err := freePort()
	if err != nil {
		return nil, err
	}
	browser := newBrowserClient("http://127.0.0.1:"+driverPort, "")
	browser.headless = cfg.Headless
	defer func() {
		if err != nil {
			browser.Close()
		}
	}()
	browser.profile, err = os.MkdirTemp("", "blazesolver-profile-")
	if err != nil {
		return nil, err
	}
	args := chromeArguments(cfg, userAgent, debugPort, browser.profile)
	if proxy != nil {
		_, hasUser := proxy["username"]
		_, hasPass := proxy["password"]
		if _, hasURL := proxy["url"]; hasURL && hasUser && hasPass {
			browser.extension, err = createProxyExtension(proxy)
			if err != nil {
				return nil, err
			}
			args = append(args, "--load-extension="+browser.extension)
			for i, arg := range args {
				if strings.HasPrefix(arg, "--disable-features=") {
					args[i] += ",DisableLoadExtensionCommandLineSwitch"
				}
			}
		} else if proxyURL, ok := proxy["url"]; ok {
			args = append(args, "--proxy-server="+fmt.Sprint(proxyURL))
		}
	}
	browser.chrome = exec.Command(chromePath, args...)
	if runtime.GOOS == "linux" && cfg.Headless {
		display, displayErr := browserDisplay(startup)
		if displayErr != nil {
			return nil, displayErr
		}
		browser.chrome.Env = append(os.Environ(), "DISPLAY="+display)
	}
	browser.chromeDone, err = startBrowserProcess(browser.chrome)
	if err != nil {
		return nil, fmt.Errorf("start Chrome: %w", err)
	}
	if err = waitHTTP(startup, "http://127.0.0.1:"+debugPort+"/json/version", browser.chromeDone); err != nil {
		return nil, fmt.Errorf("start Chrome: %w", err)
	}
	browser.driver = exec.Command(driverPath, "--port="+driverPort, "--allowed-ips=127.0.0.1")
	browser.driverDone, err = startBrowserProcess(browser.driver)
	if err != nil {
		return nil, fmt.Errorf("start ChromeDriver: %w", err)
	}
	if err = waitHTTP(startup, browser.Endpoint+"/status", browser.driverDone); err != nil {
		return nil, fmt.Errorf("start ChromeDriver: %w", err)
	}
	value, err := browser.rawCall(startup, http.MethodPost, "/session", map[string]any{"capabilities": map[string]any{"alwaysMatch": map[string]any{
		"browserName": "chrome", "pageLoadStrategy": "normal", "goog:chromeOptions": map[string]any{"debuggerAddress": "127.0.0.1:" + debugPort},
	}}})
	if err != nil {
		return nil, err
	}
	session, ok := value.(map[string]any)
	if !ok {
		return nil, errors.New("ChromeDriver returned an invalid session")
	}
	browser.SessionID, _ = session["sessionId"].(string)
	if browser.SessionID == "" {
		return nil, errors.New("ChromeDriver returned no session ID")
	}
	return browser, nil
}

func (b *Browser) Call(ctx context.Context, method, path string, payload any) (any, error) {
	return b.rawCall(ctx, method, "/session/"+b.SessionID+path, payload)
}

func (b *Browser) rawCall(ctx context.Context, method, path string, payload any) (any, error) {
	var body io.Reader
	if payload != nil {
		data, err := json.Marshal(payload)
		if err != nil {
			return nil, err
		}
		body = bytes.NewReader(data)
	}
	req, err := http.NewRequestWithContext(ctx, method, b.Endpoint+path, body)
	if err != nil {
		return nil, err
	}
	if payload != nil {
		req.Header.Set("Content-Type", "application/json;charset=UTF-8")
	}
	response, err := b.Client.Do(req)
	if err != nil {
		return nil, err
	}
	defer response.Body.Close()
	var result struct {
		Value any `json:"value"`
	}
	if err := json.NewDecoder(response.Body).Decode(&result); err != nil {
		return nil, fmt.Errorf("ChromeDriver response: %w", err)
	}
	if response.StatusCode >= 400 {
		if value, ok := result.Value.(map[string]any); ok {
			if kind, ok := value["error"].(string); ok && kind != "" {
				message, _ := value["message"].(string)
				// Existing clients see Selenium's exception text, including this
				// prefix. Keep it even though Selenium is no longer in the runtime.
				formatted := "Message: " + message + "\n"
				if stack, ok := value["stacktrace"].(string); ok && stack != "" {
					formatted += "Stacktrace:\n" + stack
				}
				return nil, errors.New(formatted)
			}
		}
		return nil, fmt.Errorf("ChromeDriver returned HTTP %d", response.StatusCode)
	}
	return result.Value, nil
}

func (b *Browser) Execute(ctx context.Context, script string, args ...any) (any, error) {
	if args == nil {
		args = []any{}
	}
	return b.Call(ctx, http.MethodPost, "/execute/sync", map[string]any{"script": script, "args": args})
}

func (b *Browser) CDP(ctx context.Context, command string, params any) (any, error) {
	return b.Call(ctx, http.MethodPost, "/goog/cdp/execute", map[string]any{"cmd": command, "params": params})
}

func (b *Browser) Navigate(ctx context.Context, address string) error {
	if b.headless {
		webdriver, err := b.Execute(ctx, "return navigator.webdriver")
		if err != nil {
			return err
		}
		if webdriver == true {
			if _, err = b.CDP(ctx, "Page.addScriptToEvaluateOnNewDocument", map[string]any{"source": headlessNavigatorScript}); err != nil {
				return err
			}
			userAgent, err := b.Execute(ctx, "return navigator.userAgent")
			if err != nil {
				return err
			}
			if _, err = b.CDP(ctx, "Network.setUserAgentOverride", map[string]any{"userAgent": strings.ReplaceAll(fmt.Sprint(userAgent), "Headless", "")}); err != nil {
				return err
			}
			if _, err = b.CDP(ctx, "Page.addScriptToEvaluateOnNewDocument", map[string]any{"source": headlessBrowserScript}); err != nil {
				return err
			}
		}
	}
	_, err := b.Call(ctx, http.MethodPost, "/url", map[string]any{"url": address})
	return err
}

func (b *Browser) Close() error {
	b.closeOnce.Do(func() {
		if b.SessionID != "" {
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			b.CDP(ctx, "Browser.close", map[string]any{})
			// Browser.close can invalidate the attached WebDriver session. A
			// subsequent DELETE is best-effort cleanup, not a request failure.
			b.Call(ctx, http.MethodDelete, "", nil)
			cancel()
		}
		if b.chromeDone != nil {
			select {
			case <-b.chromeDone:
			case <-time.After(time.Second):
			}
		}
		stopBrowserProcess(b.chrome, b.chromeDone)
		stopBrowserProcess(b.driver, b.driverDone)
		if b.Client != nil {
			b.Client.CloseIdleConnections()
		}
		if b.profile != "" {
			os.RemoveAll(b.profile)
		}
		if b.extension != "" {
			os.RemoveAll(b.extension)
		}
	})
	return nil
}

func startBrowserProcess(command *exec.Cmd) (chan error, error) {
	if err := command.Start(); err != nil {
		return nil, err
	}
	done := make(chan error, 1)
	go func() { done <- command.Wait(); close(done) }()
	return done, nil
}

func stopBrowserProcess(command *exec.Cmd, done chan error) {
	if command == nil || command.Process == nil {
		return
	}
	select {
	case <-done:
		return
	default:
	}
	command.Process.Kill()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
	}
}

func freePort() (string, error) {
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		return "", err
	}
	defer listener.Close()
	return strconv.Itoa(listener.Addr().(*net.TCPAddr).Port), nil
}

func waitHTTP(ctx context.Context, address string, processDone <-chan error) error {
	client := &http.Client{Timeout: time.Second, Transport: &http.Transport{Proxy: nil}}
	defer client.CloseIdleConnections()
	timer := time.NewTicker(25 * time.Millisecond)
	defer timer.Stop()
	for {
		request, err := http.NewRequestWithContext(ctx, http.MethodGet, address, nil)
		if err != nil {
			return err
		}
		if response, err := client.Do(request); err == nil {
			response.Body.Close()
			if response.StatusCode == http.StatusOK {
				return nil
			}
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case err := <-processDone:
			return fmt.Errorf("process exited: %v", err)
		case <-timer.C:
		}
	}
}

var disabledChromeFeatures = []string{
	"LocalNetworkAccessChecks", "OptimizationGuideOnDeviceModel", "GlicActor", "GlicActorUi", "PromptAPIForGeminiNano", "PromptAPIX", "PromptAPINoTranslation", "SummarizationAPIForGeminiNano", "WriterAPIForGeminiNano", "TranslationAPI", "LanguageDetectionAPI", "AIPromptAPI", "AIRewriteAPI", "AIWritingAssistant", "AIGenerativeSuggestions", "AIGeneratedFlags", "AICompositionFeature", "HistoryAI", "TabOrganizer", "SmartSuggestion",
}

func chromeArguments(cfg Config, userAgent, debugPort, profile string) []string {
	args := []string{"--no-sandbox", "--window-size=1920,1080", "--disable-search-engine-choice-screen", "--disable-setuid-sandbox", "--disable-dev-shm-usage", "--no-zygote", "--ignore-certificate-errors", "--ignore-ssl-errors", "--disable-features=" + strings.Join(disabledChromeFeatures, ",")}
	if runtime.GOARCH == "arm" || runtime.GOARCH == "arm64" {
		args = append(args, "--disable-gpu-sandbox")
	}
	language := cfg.Language
	if language != "" {
		args = append(args, "--accept-lang="+language)
	} else {
		language = strings.Split(strings.Split(os.Getenv("LANG"), ".")[0], "@")[0]
		language = strings.ReplaceAll(language, "_", "-")
		if language == "" || language == "C" {
			language = "en-US"
		}
	}
	if userAgent != "" {
		args = append(args, "--user-agent="+userAgent)
	}
	args = append(args, "--remote-debugging-host=127.0.0.1", "--remote-debugging-port="+debugPort, "--user-data-dir="+profile, "--lang="+language, "--no-default-browser-check", "--no-first-run", "--no-sandbox", "--test-type", "--window-size=1920,1080", "--start-maximized", "--no-sandbox", "--log-level=0")
	if cfg.Headless {
		args = append(args, "--headless=new")
	}
	return args
}

func findChrome(configured string) (string, error) {
	if configured != "" {
		return exec.LookPath(configured)
	}
	executable, _ := os.Executable()
	candidates := []string{filepath.Join(filepath.Dir(executable), "chrome", "chrome"), filepath.Join("src", "chrome", "chrome"), "chromium", "chromium-browser", "google-chrome", "google-chrome-stable", "chrome", "/Applications/Google Chrome.app/Contents/MacOS/Google Chrome", "/Applications/Chromium.app/Contents/MacOS/Chromium"}
	if runtime.GOOS == "windows" {
		candidates = append([]string{filepath.Join(filepath.Dir(executable), "chrome", "chrome.exe"), filepath.Join("src", "chrome", "chrome.exe")}, candidates...)
		for _, base := range []string{os.Getenv("PROGRAMFILES"), os.Getenv("PROGRAMFILES(X86)"), os.Getenv("LOCALAPPDATA")} {
			candidates = append(candidates, filepath.Join(base, "Google", "Chrome", "Application", "chrome.exe"))
		}
	}
	for _, candidate := range candidates {
		if path, err := exec.LookPath(candidate); err == nil {
			return path, nil
		}
	}
	return "", errors.New("Chrome / Chromium web browser not installed!")
}

var driverPreparation sync.Mutex
var preparedDriverPaths = map[string]string{}
var injectionBlock = regexp.MustCompile(`\{window\.cdc.*?;\}`)

func patchDriver(content []byte) ([]byte, error) {
	if bytes.Contains(content, []byte("undetected chromedriver")) {
		return content, nil
	}
	match := injectionBlock.FindIndex(content)
	if match == nil {
		return nil, errors.New("ChromeDriver injection block not found; refusing to run an unpatched driver")
	}
	replacement := []byte(`{console.log("undetected chromedriver 1337!")}`)
	if match[1]-match[0] < len(replacement) {
		return nil, errors.New("ChromeDriver injection block is too short")
	}
	padded := bytes.Repeat([]byte(" "), match[1]-match[0])
	copy(padded, replacement)
	return bytes.ReplaceAll(content, content[match[0]:match[1]], padded), nil
}

func prepareDriver(ctx context.Context, cfg Config, chromePath string) (path string, err error) {
	driverPreparation.Lock()
	defer driverPreparation.Unlock()
	key := cfg.CacheDir + "\x00" + cfg.DriverPath + "\x00" + chromePath
	if cached := preparedDriverPaths[key]; cached != "" {
		if _, statErr := os.Stat(cached); statErr == nil {
			return cached, nil
		}
	}
	defer func() {
		if err == nil {
			preparedDriverPaths[key] = path
		}
	}()
	cache := cfg.CacheDir
	if cache == "" {
		base, err := os.UserCacheDir()
		if err != nil {
			return "", err
		}
		cache = filepath.Join(base, "blazesolver")
	}
	if err := os.MkdirAll(cache, 0700); err != nil {
		return "", err
	}
	source := cfg.DriverPath
	if source == "" {
		if _, err := os.Stat("/app/chromedriver"); err == nil {
			source = "/app/chromedriver"
		}
	}
	if source == "" {
		source, _ = exec.LookPath("chromedriver")
	}
	if source != "" {
		data, err := os.ReadFile(source)
		if err != nil {
			return "", err
		}
		digest := sha256.Sum256(data)
		destination := filepath.Join(cache, fmt.Sprintf("chromedriver-%x%s", digest[:8], driverSuffix()))
		if _, err := os.Stat(destination); err == nil {
			return destination, nil
		}
		patched, err := patchDriver(data)
		if err != nil {
			return "", err
		}
		return destination, atomicDriverWrite(destination, patched)
	}
	versionCommand := exec.CommandContext(ctx, chromePath, "--version")
	if runtime.GOOS == "windows" {
		versionCommand = exec.CommandContext(ctx, "powershell", "-NoProfile", "-NonInteractive", "-Command", "(Get-Item -LiteralPath $env:BLAZESOLVER_CHROME_VERSION_PATH).VersionInfo.ProductVersion")
		versionCommand.Env = append(os.Environ(), "BLAZESOLVER_CHROME_VERSION_PATH="+chromePath)
	}
	output, err := versionCommand.Output()
	if err != nil {
		return "", fmt.Errorf("detect Chrome version: %w", err)
	}
	major := regexp.MustCompile(`\d+`).FindString(string(output))
	if major == "" {
		return "", errors.New("Chrome / Chromium version not detected!")
	}
	destination := filepath.Join(cache, "chromedriver-"+runtime.GOOS+"-"+runtime.GOARCH+"-"+major+driverSuffix())
	if data, err := os.ReadFile(destination); err == nil && bytes.Contains(data, []byte("undetected chromedriver")) {
		return destination, nil
	}
	data, err := downloadDriver(ctx, major)
	if err != nil {
		return "", err
	}
	patched, err := patchDriver(data)
	if err != nil {
		return "", err
	}
	return destination, atomicDriverWrite(destination, patched)
}

func driverSuffix() string {
	if runtime.GOOS == "windows" {
		return ".exe"
	}
	return ""
}

func atomicDriverWrite(destination string, data []byte) error {
	file, err := os.CreateTemp(filepath.Dir(destination), "driver-")
	if err != nil {
		return err
	}
	defer os.Remove(file.Name())
	if _, err = file.Write(data); err != nil {
		file.Close()
		return err
	}
	if err = file.Chmod(0755); err != nil {
		file.Close()
		return err
	}
	if err = file.Close(); err != nil {
		return err
	}
	if runtime.GOOS == "darwin" && runtime.GOARCH == "arm64" {
		binary, parseErr := macho.NewFile(bytes.NewReader(data))
		if parseErr == nil && binary.Cpu == macho.CpuArm64 {
			if output, signErr := exec.Command("/usr/bin/codesign", "--force", "--sign", "-", file.Name()).CombinedOutput(); signErr != nil {
				return fmt.Errorf("sign patched ARM64 ChromeDriver: %w: %s", signErr, output)
			}
		}
	}
	return os.Rename(file.Name(), destination)
}

func fetchBrowserResource(ctx context.Context, address string) ([]byte, error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, address, nil)
	if err != nil {
		return nil, err
	}
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		return nil, err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("download %s: HTTP %d", address, response.StatusCode)
	}
	return io.ReadAll(io.LimitReader(response.Body, 256<<20))
}

func downloadDriver(ctx context.Context, major string) ([]byte, error) {
	platform := ""
	switch runtime.GOOS {
	case "darwin":
		platform = "mac-x64"
		if runtime.GOARCH == "arm64" {
			platform = "mac-arm64"
		}
	case "linux":
		platform = "linux64"
	case "windows":
		platform = "win32"
		if runtime.GOARCH == "amd64" {
			platform = "win64"
		}
	default:
		return nil, fmt.Errorf("automatic driver download unsupported on %s; configure DRIVER_PATH", runtime.GOOS)
	}
	if runtime.GOOS == "linux" && runtime.GOARCH != "amd64" {
		return nil, errors.New("install a system ChromeDriver on Linux ARM and configure DRIVER_PATH")
	}
	if milestone, _ := strconv.Atoi(major); milestone < 115 {
		if runtime.GOOS == "darwin" {
			platform = "mac64"
		}
		if runtime.GOOS == "windows" {
			platform = "win32"
		}
		version, err := fetchBrowserResource(ctx, "https://chromedriver.storage.googleapis.com/LATEST_RELEASE_"+major)
		if err != nil {
			return nil, err
		}
		archive, err := fetchBrowserResource(ctx, "https://chromedriver.storage.googleapis.com/"+strings.TrimSpace(string(version))+"/chromedriver_"+platform+".zip")
		if err != nil {
			return nil, err
		}
		return driverFromArchive(archive)
	}
	metadata, err := fetchBrowserResource(ctx, "https://googlechromelabs.github.io/chrome-for-testing/latest-versions-per-milestone-with-downloads.json")
	if err != nil {
		return nil, err
	}
	var catalog struct {
		Milestones map[string]struct {
			Downloads map[string][]struct{ Platform, URL string }
		}
	}
	if err = json.Unmarshal(metadata, &catalog); err != nil {
		return nil, err
	}
	address := ""
	for _, download := range catalog.Milestones[major].Downloads["chromedriver"] {
		if download.Platform == platform {
			address = download.URL
			break
		}
	}
	if address == "" {
		return nil, fmt.Errorf("ChromeDriver download unavailable for Chrome %s on %s", major, platform)
	}
	data, err := fetchBrowserResource(ctx, address)
	if err != nil {
		return nil, err
	}
	return driverFromArchive(data)
}

func driverFromArchive(data []byte) ([]byte, error) {
	archive, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		return nil, err
	}
	for _, file := range archive.File {
		if filepath.Base(file.Name) == "chromedriver"+driverSuffix() {
			reader, err := file.Open()
			if err != nil {
				return nil, err
			}
			defer reader.Close()
			return io.ReadAll(io.LimitReader(reader, 256<<20))
		}
	}
	return nil, errors.New("ChromeDriver archive contains no executable")
}

func createProxyExtension(proxy map[string]any) (string, error) {
	parsed, err := url.Parse(fmt.Sprint(proxy["url"]))
	if err != nil {
		return "", err
	}
	var port any
	if parsed.Port() != "" {
		number, err := strconv.Atoi(parsed.Port())
		if err != nil {
			return "", err
		}
		port = number
	}
	values := []any{parsed.Scheme, parsed.Hostname(), port, proxy["username"], proxy["password"]}
	encoded := make([]string, len(values))
	for i, value := range values {
		data, err := json.Marshal(value)
		if err != nil {
			return "", err
		}
		encoded[i] = string(data)
	}
	script := fmt.Sprintf(`var config={mode:"fixed_servers",rules:{singleProxy:{scheme:%s,host:%s,port:%s},bypassList:["localhost"]}};
chrome.proxy.settings.set({value:config,scope:"regular"},function(){});
function callbackFn(details){return {authCredentials:{username:%s,password:%s}};}
chrome.webRequest.onAuthRequired.addListener(callbackFn,{urls:["<all_urls>"]},['blocking']);`, encoded[0], encoded[1], encoded[2], encoded[3], encoded[4])
	directory, err := os.MkdirTemp("", "blazesolver-proxy-")
	if err != nil {
		return "", err
	}
	manifest := `{"version":"1.0.0","manifest_version":3,"name":"Chrome Proxy","permissions":["proxy","tabs","storage","webRequest","webRequestAuthProvider"],"host_permissions":["<all_urls>"],"background":{"service_worker":"background.js"},"minimum_chrome_version":"76.0.0"}`
	for name, content := range map[string]string{"manifest.json": manifest, "background.js": script} {
		if err = os.WriteFile(filepath.Join(directory, name), []byte(content), 0600); err != nil {
			os.RemoveAll(directory)
			return "", err
		}
	}
	return directory, nil
}

var virtualDisplay struct {
	sync.Mutex
	address string
	process *exec.Cmd
	done    chan error
}

func browserDisplay(ctx context.Context) (string, error) {
	virtualDisplay.Lock()
	defer virtualDisplay.Unlock()
	if virtualDisplay.address != "" {
		select {
		case <-virtualDisplay.done:
			virtualDisplay.address = ""
		default:
			return virtualDisplay.address, nil
		}
	}
	reader, writer, err := os.Pipe()
	if err != nil {
		return "", err
	}
	defer reader.Close()
	command := exec.Command("Xvfb", "-displayfd", "3", "-screen", "0", "800x600x24")
	command.ExtraFiles = []*os.File{writer}
	done, err := startBrowserProcess(command)
	writer.Close()
	if err != nil {
		return "", fmt.Errorf("start Xvfb: %w", err)
	}
	display := make(chan string, 1)
	go func() { var number string; fmt.Fscanln(reader, &number); display <- number }()
	select {
	case number := <-display:
		if number == "" {
			stopBrowserProcess(command, done)
			return "", errors.New("Xvfb returned no display number")
		}
		virtualDisplay.address = ":" + number
		virtualDisplay.process = command
		virtualDisplay.done = done
		return virtualDisplay.address, nil
	case <-ctx.Done():
		stopBrowserProcess(command, done)
		return "", ctx.Err()
	case err := <-done:
		return "", fmt.Errorf("Xvfb exited: %v", err)
	}
}

func StopBrowserDisplay() {
	virtualDisplay.Lock()
	defer virtualDisplay.Unlock()
	stopBrowserProcess(virtualDisplay.process, virtualDisplay.done)
	virtualDisplay.address = ""
}

const headlessNavigatorScript = `
Object.defineProperty(window, "navigator", {
  value: new Proxy(navigator, {
    has: (target, key) => (key === "webdriver" ? false : key in target),
    get: (target, key) =>
      key === "webdriver"
        ? false
        : typeof target[key] === "function"
        ? target[key].bind(target)
        : target[key],
  }),
});
`

const headlessBrowserScript = `
Object.defineProperty(navigator, 'maxTouchPoints', {get: () => 1});
Object.defineProperty(navigator.connection, 'rtt', {get: () => 100});

// https://github.com/microlinkhq/browserless/blob/master/packages/goto/src/evasions/chrome-runtime.js
window.chrome = {
    app: {
        isInstalled: false,
        InstallState: {
            DISABLED: 'disabled',
            INSTALLED: 'installed',
            NOT_INSTALLED: 'not_installed'
        },
        RunningState: {
            CANNOT_RUN: 'cannot_run',
            READY_TO_RUN: 'ready_to_run',
            RUNNING: 'running'
        }
    },
    runtime: {
        OnInstalledReason: {
            CHROME_UPDATE: 'chrome_update',
            INSTALL: 'install',
            SHARED_MODULE_UPDATE: 'shared_module_update',
            UPDATE: 'update'
        },
        OnRestartRequiredReason: {
            APP_UPDATE: 'app_update',
            OS_UPDATE: 'os_update',
            PERIODIC: 'periodic'
        },
        PlatformArch: {
            ARM: 'arm',
            ARM64: 'arm64',
            MIPS: 'mips',
            MIPS64: 'mips64',
            X86_32: 'x86-32',
            X86_64: 'x86-64'
        },
        PlatformNaclArch: {
            ARM: 'arm',
            MIPS: 'mips',
            MIPS64: 'mips64',
            X86_32: 'x86-32',
            X86_64: 'x86-64'
        },
        PlatformOs: {
            ANDROID: 'android',
            CROS: 'cros',
            LINUX: 'linux',
            MAC: 'mac',
            OPENBSD: 'openbsd',
            WIN: 'win'
        },
        RequestUpdateCheckStatus: {
            NO_UPDATE: 'no_update',
            THROTTLED: 'throttled',
            UPDATE_AVAILABLE: 'update_available'
        }
    }
}

// https://github.com/microlinkhq/browserless/blob/master/packages/goto/src/evasions/navigator-permissions.js
if (!window.Notification) {
    window.Notification = {
        permission: 'denied'
    }
}

const originalQuery = window.navigator.permissions.query
window.navigator.permissions.__proto__.query = parameters =>
    parameters.name === 'notifications'
        ? Promise.resolve({ state: window.Notification.permission })
        : originalQuery(parameters)

const oldCall = Function.prototype.call
function call() {
    return oldCall.apply(this, arguments)
}
Function.prototype.call = call

const nativeToStringFunctionString = Error.toString().replace(/Error/g, 'toString')
const oldToString = Function.prototype.toString

function functionToString() {
    if (this === window.navigator.permissions.query) {
        return 'function query() { [native code] }'
    }
    if (this === functionToString) {
        return nativeToStringFunctionString
    }
    return oldCall.call(oldToString, this)
}
// eslint-disable-next-line
Function.prototype.toString = functionToString
`
