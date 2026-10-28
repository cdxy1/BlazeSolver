# HTTP API reference

HTTP commands and environment configuration for BlazeSolver.

## Usage

Example Bash request:

```bash
curl -L -X POST 'http://localhost:8191/v1' \
-H 'Content-Type: application/json' \
--data-raw '{
  "cmd": "request.get",
  "url": "http://www.google.com/",
  "maxTimeout": 60000
}'
```

Example Python request:

```py
import requests

url = "http://localhost:8191/v1"
headers = {"Content-Type": "application/json"}
data = {
    "cmd": "request.get",
    "url": "http://www.google.com/",
    "maxTimeout": 60000
}
response = requests.post(url, headers=headers, json=data)
print(response.text)
```

Example PowerShell request:

```ps1
$body = @{
    cmd = "request.get"
    url = "http://www.google.com/"
    maxTimeout = 60000
} | ConvertTo-Json

irm -UseBasicParsing 'http://localhost:8191/v1' -Headers @{"Content-Type"="application/json"} -Method Post -Body $body
```

### Commands

#### + `sessions.create`

This will launch a new browser instance which will retain cookies until you destroy it with `sessions.destroy`.
This comes in handy, so you don't have to keep solving challenges over and over and you won't need to keep sending
cookies for the browser to use.

This also speeds up the requests since it won't have to launch a new browser instance for every request.

| Parameter | Notes                                                                                                                                                                                                                                                                                                             |
| --------- | ----------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| session   | Optional. The session ID that you want to be assigned to the instance. If isn't set a random UUID will be assigned.                                                                                                                                                                                               |
| proxy     | Optional, default disabled. Eg: `"proxy": {"url": "http://127.0.0.1:8888"}`. You must include the proxy schema in the URL: `http://`, `socks4://` or `socks5://`. Authorization (username/password) is supported. Eg: `"proxy": {"url": "http://127.0.0.1:8888", "username": "testuser", "password": "testpass"}` |

#### + `sessions.list`

Returns a list of all the active sessions. More for debugging if you are curious to see how many sessions are running.
You should always make sure to properly close each session when you are done using them as too many may slow your
computer down.

Example response:

```json
{
  "sessions": ["session_id_1", "session_id_2", "session_id_3..."]
}
```

#### + `sessions.destroy`

This will properly shutdown a browser instance and remove all files associated with it to free up resources for a new
session. When you no longer need to use a session you should make sure to close it.

| Parameter | Notes                                         |
| --------- | --------------------------------------------- |
| session   | The session ID that you want to be destroyed. |

#### + `request.get`

| Parameter           | Notes                                                                                                                                                                                                                                                                                                                                        |
| ------------------- | -------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| url                 | Mandatory                                                                                                                                                                                                                                                                                                                                    |
| session             | Optional. Will send the request from and existing browser instance. If one is not sent it will create a temporary instance that will be destroyed immediately after the request is completed.                                                                                                                                                |
| session_ttl_minutes | Optional. BlazeSolver will automatically rotate expired sessions based on the TTL provided in minutes.                                                                                                                                                                                                                                      |
| maxTimeout          | Optional, default value 60000. Max timeout to solve the challenge in milliseconds.                                                                                                                                                                                                                                                           |
| cookies             | Optional. Will be used by the headless browser. Eg: `"cookies": [{"name": "cookie1", "value": "value1"}, {"name": "cookie2", "value": "value2"}]`.                                                                                                                                                                                           |
| returnOnlyCookies   | Optional, default false. Only returns the cookies. Response data, headers and other parts of the response are removed.                                                                                                                                                                                                                       |
| returnScreenshot    | Optional, default false. Captures a screenshot of the final rendered page after all challenges and waits are completed. The screenshot is returned as a Base64-encoded PNG string in the `screenshot` field of the response.                                                                                                                 |
| proxy               | Optional, default disabled. Eg: `"proxy": {"url": "http://127.0.0.1:8888"}`. You must include the proxy schema in the URL: `http://`, `socks4://` or `socks5://`. Authorization (username/password) is not supported. (When the `session` parameter is set, the proxy is ignored; a session specific proxy can be set in `sessions.create`.) |
| waitInSeconds       | Optional, default none. Length to wait in seconds after solving the challenge, and before returning the results. Useful to allow it to load dynamic content.                                                                                                                                                                                 |
| disableMedia        | Optional, default false. When true BlazeSolver will prevent media resources (images, CSS, and fonts) from being loaded to speed up navigation.                                                                                                                                                                                              |
| tabs_till_verify    | Optional, default none. Number of times the `Tab` button is needed to be pressed to end up on the turnstile captcha, in order to verify it. After verifying the captcha, the result will be stored in the solution under `turnstile_token`.                                                                                                  |

> **Warning**
> If you want to use Cloudflare clearance cookie in your scripts, make sure you use the BlazeSolver User-Agent too. If they don't match you will see the challenge.

Example response from running the `curl` above:

```json
{
  "solution": {
    "url": "https://www.google.com/?gws_rd=ssl",
    "status": 200,
    "headers": {
      "status": "200",
      "date": "Thu, 16 Jul 2020 04:15:49 GMT",
      "expires": "-1",
      "cache-control": "private, max-age=0",
      "content-type": "text/html; charset=UTF-8",
      "strict-transport-security": "max-age=31536000",
      "p3p": "CP=\"This is not a P3P policy! See g.co/p3phelp for more info.\"",
      "content-encoding": "br",
      "server": "gws",
      "content-length": "61587",
      "x-xss-protection": "0",
      "x-frame-options": "SAMEORIGIN",
      "set-cookie": "1P_JAR=2020-07-16-04; expires=Sat..."
    },
    "response": "<!DOCTYPE html>...",
    "cookies": [
      {
        "name": "NID",
        "value": "204=QE3Ocq15XalczqjuDy52HeseG3zAZuJzID3R57...",
        "domain": ".google.com",
        "path": "/",
        "expires": 1610684149.307722,
        "size": 178,
        "httpOnly": true,
        "secure": true,
        "session": false,
        "sameSite": "None"
      },
      {
        "name": "1P_JAR",
        "value": "2020-07-16-04",
        "domain": ".google.com",
        "path": "/",
        "expires": 1597464949.307626,
        "size": 19,
        "httpOnly": false,
        "secure": true,
        "session": false,
        "sameSite": "None"
      }
    ],
    "userAgent": "Windows NT 10.0; Win64; x64) AppleWebKit/5...",
    "turnstile_token": "03AGdBq24k3lK7JH2v8uN1T5F..."
  },
  "status": "ok",
  "message": "",
  "startTimestamp": 1594872947467,
  "endTimestamp": 1594872949617,
  "version": "1.0.0"
}
```

### + `request.post`

This works like `request.get`, with the addition of the postData parameter. Note that `tabs_till_verify` is currently supported only for GET requests and requires one extra argument.

| Parameter | Notes                                                                    |
| --------- | ------------------------------------------------------------------------ |
| postData  | Must be a string with `application/x-www-form-urlencoded`. Eg: `a=b&c=d` |

## Environment variables

| Name                 | Default                | Notes                                                                                                                                    |
|----------------------| ---------------------- | ---------------------------------------------------------------------------------------------------------------------------------------- |
| LOG_LEVEL            | info                   | Verbosity of the logging. Use `LOG_LEVEL=debug` for more information.                                                                    |
| LOG_FILE             | none                   | Path to capture log to file. Example: `/config/blazesolver.log`.                                                                         |
| LOG_HTML             | false                  | Only for debugging. If `true` all HTML that passes through the proxy will be logged to the console in `debug` level.                     |
| PROXY_URL            | none                   | URL for proxy. Will be overwritten by `request` or `sessions` proxy, if used. Example: `http://127.0.0.1:8080`.                          |
| PROXY_USERNAME       | none                   | Username for proxy. Will be overwritten by `request` or `sessions` proxy, if used. Example: `testuser`.                                  |
| PROXY_PASSWORD       | none                   | Password for proxy. Will be overwritten by `request` or `sessions` proxy, if used. Example: `testpass`.                                  |
| CAPTCHA_SOLVER       | none                   | Captcha solving method. It is used when a captcha is encountered. See the Captcha Solvers section.                                       |
| TZ                   | UTC                    | Timezone used in the logs and the web browser. Example: `TZ=Europe/London`.                                                              |
| LANG                 | none                   | Language used in the web browser. Example: `LANG=en_GB`.                                                                                 |
| HEADLESS             | true                   | Only for debugging. To run the web browser in headless mode or visible.                                                                  |
| DISABLE_MEDIA        | false                  | To disable loading images, CSS, and other media in the web browser to save network bandwidth.                                            |
| BROWSER_WAIT_TIMEOUT | 1                    | Seconds to wait for the web browser to reach an expected page state on each attempt. Increase it on slow hosts or slow websites.         |
| TEST_URL             | https://www.google.com | BlazeSolver makes a request on start to make sure the web browser is working. You can change that URL if it is blocked in your country. |
| PORT                 | 8191                   | Listening port. You don't need to change this if you are running on Docker.                                                              |
| HOST                 | 0.0.0.0                | Listening interface. You don't need to change this if you are running on Docker.                                                         |
| PROMETHEUS_ENABLED   | false                  | Enable Prometheus exporter. See the Prometheus section below.                                                                            |
| PROMETHEUS_PORT      | 8192                   | Listening port for Prometheus exporter. See the Prometheus section below.                                                                |

Environment variables are set differently depending on the operating system. Some examples:

- Docker: Take a look at the Docker section in this document. Environment variables can be set in the `docker-compose.yml` file or in the Docker CLI command.
- Linux: Run `export LOG_LEVEL=debug` and then run `blazesolver` in the same shell.
- Windows: Open `cmd.exe`, run `set LOG_LEVEL=debug` and then run `blazesolver.exe` in the same shell.

## Prometheus exporter

The Prometheus exporter for BlazeSolver is disabled by default. It can be enabled with the environment variable `PROMETHEUS_ENABLED`. If you are using Docker make sure you expose the `PROMETHEUS_PORT`.

Example metrics:

```shell
# HELP blazesolver_request_total Total requests with result
# TYPE blazesolver_request_total counter
blazesolver_request_total{domain="nowsecure.nl",result="solved"} 1.0
# HELP blazesolver_request_created Total requests with result
# TYPE blazesolver_request_created gauge
blazesolver_request_created{domain="nowsecure.nl",result="solved"} 1.690141657157109e+09
# HELP blazesolver_request_duration Request duration in seconds
# TYPE blazesolver_request_duration histogram
blazesolver_request_duration_bucket{domain="nowsecure.nl",le="0.0"} 0.0
blazesolver_request_duration_bucket{domain="nowsecure.nl",le="10.0"} 1.0
blazesolver_request_duration_bucket{domain="nowsecure.nl",le="25.0"} 1.0
blazesolver_request_duration_bucket{domain="nowsecure.nl",le="50.0"} 1.0
blazesolver_request_duration_bucket{domain="nowsecure.nl",le="+Inf"} 1.0
blazesolver_request_duration_count{domain="nowsecure.nl"} 1.0
blazesolver_request_duration_sum{domain="nowsecure.nl"} 5.858
# HELP blazesolver_request_duration_created Request duration in seconds
# TYPE blazesolver_request_duration_created gauge
blazesolver_request_duration_created{domain="nowsecure.nl"} 1.6901416571570296e+09
```

## Challenge handling

Browser challenges are handled by Chromium with the patched driver. Optional
`tabs_till_verify` controls the tab presses before Turnstile verification.
`returnScreenshot` adds a base64 PNG; `disableMedia` controls resource blocking.
There are no external CAPTCHA adapter modules.
