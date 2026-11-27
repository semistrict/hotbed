#!/usr/bin/env python3
import http.client
import json
import os
import re
import shutil
import socket
import subprocess
import sys
import urllib.parse


class UnixHTTPConnection(http.client.HTTPConnection):
    def __init__(self, sock_path):
        super().__init__("localhost")
        self._sock_path = sock_path

    def connect(self):
        s = socket.socket(socket.AF_UNIX, socket.SOCK_STREAM)
        s.connect(self._sock_path)
        self.sock = s


def sock_path():
    p = os.environ.get("HOTBED_GIT_CRED_SOCK", "").strip()
    if not p:
        sys.stderr.write("hotbed-git-auth: HOTBED_GIT_CRED_SOCK is not set\n")
        sys.exit(1)
    return p


def socket_json(method, path, payload=None):
    conn = UnixHTTPConnection(sock_path())
    body = json.dumps(payload).encode("utf-8") if payload is not None else None
    headers = {"Content-Type": "application/json"} if body is not None else {}
    try:
        conn.request(method, path, body=body, headers=headers)
        resp = conn.getresponse()
        raw = resp.read().decode("utf-8", errors="replace")
    except OSError as e:
        sys.stderr.write(f"hotbed-git-auth: cannot reach worker git-auth socket: {e}\n")
        sys.exit(1)
    finally:
        conn.close()
    try:
        data = json.loads(raw) if raw else {}
    except json.JSONDecodeError:
        data = {"error": raw}
    if resp.status >= 400:
        msg = data.get("error") or raw or f"HTTP {resp.status}"
        sys.stderr.write(f"hotbed-git-auth: {msg}\n")
        sys.exit(1)
    return data


def proxy_api(method, path, host="github.com", repo="", headers=None, body=""):
    if not path.startswith("/"):
        path = "/" + path
    return socket_json("POST", "/gh-api", {
        "method": method,
        "path": path,
        "host": host or "github.com",
        "repo": repo or "",
        "headers": headers or [],
        "body": body or "",
    })


def parse_remote_url(url):
    url = url.strip()
    m = re.match(r"^https?://([^/]+)/([^/]+)/([^/]+?)(?:\.git)?/?$", url)
    if m:
        return m.group(1).lower(), f"{m.group(2)}/{m.group(3)}"
    m = re.match(r"^ssh://(?:[^@]+@)?([^/]+)/([^/]+)/([^/]+?)(?:\.git)?/?$", url)
    if m:
        return m.group(1).lower(), f"{m.group(2)}/{m.group(3)}"
    m = re.match(r"^(?:[^@]+@)?([^:]+):/?([^/]+)/([^/]+?)(?:\.git)?/?$", url)
    if m:
        return m.group(1).lower(), f"{m.group(2)}/{m.group(3)}"
    return "", ""


def detect_repo(explicit=""):
    if explicit:
        s = explicit.strip().strip("/")
        if s.endswith(".git"):
            s = s[:-4]
        parts = s.split("/")
        if len(parts) == 3:
            return parts[0].lower(), f"{parts[1]}/{parts[2]}"
        if len(parts) == 2:
            return "github.com", f"{parts[0]}/{parts[1]}"
        return "github.com", s
    env_repo = os.environ.get("GH_REPO", "").strip()
    if env_repo:
        return detect_repo(env_repo)
    try:
        out = subprocess.check_output(
            ["git", "remote", "get-url", "origin"],
            stderr=subprocess.DEVNULL,
            text=True,
        ).strip()
        host, repo = parse_remote_url(out)
        if repo:
            return host or "github.com", repo
    except Exception:
        pass
    return "github.com", ""


def detect_branch():
    try:
        return subprocess.check_output(
            ["git", "rev-parse", "--abbrev-ref", "HEAD"],
            stderr=subprocess.DEVNULL,
            text=True,
        ).strip()
    except Exception:
        return ""


def parse_typed_value(val):
    if val == "true":
        return True
    if val == "false":
        return False
    if val == "null":
        return None
    if val.startswith("@"):
        p = val[1:]
        if p == "-":
            return sys.stdin.read()
        with open(p, "r", encoding="utf-8") as f:
            return f.read()
    try:
        return int(val)
    except ValueError:
        pass
    try:
        return float(val)
    except ValueError:
        pass
    return val


def run_jq(text, expr):
    jq_bin = shutil.which("jq")
    if jq_bin:
        p = subprocess.run([jq_bin, "-r", expr], input=text, text=True, capture_output=True)
        if p.returncode != 0:
            sys.stderr.write(p.stderr)
            sys.exit(p.returncode)
        return p.stdout
    # Simple fallback when jq binary is absent: support .a.b lookups
    obj = json.loads(text)
    cur = obj
    for part in [x for x in expr.strip().lstrip(".").split(".") if x]:
        if isinstance(cur, dict):
            cur = cur.get(part)
        elif isinstance(cur, list) and part.isdigit():
            cur = cur[int(part)]
        else:
            cur = None
    if isinstance(cur, (dict, list)):
        return json.dumps(cur) + "\n"
    if cur is None:
        return "null\n"
    return str(cur) + "\n"


def project_gh_field(item, field):
    if field in item:
        return item[field]
    mapping = {
        "headRefName": lambda x: (x.get("head") or {}).get("ref"),
        "baseRefName": lambda x: (x.get("base") or {}).get("ref"),
        "isDraft": lambda x: x.get("draft"),
        "createdAt": lambda x: x.get("created_at"),
        "updatedAt": lambda x: x.get("updated_at"),
        "closedAt": lambda x: x.get("closed_at"),
        "mergedAt": lambda x: x.get("merged_at"),
        "author": lambda x: x.get("user"),
        "url": lambda x: x.get("html_url") or x.get("url"),
        "nameWithOwner": lambda x: x.get("full_name"),
        "isPrivate": lambda x: x.get("private"),
        "defaultBranchRef": lambda x: {"name": x.get("default_branch")} if x.get("default_branch") else None,
        "databaseId": lambda x: x.get("id"),
        "headBranch": lambda x: x.get("head_branch"),
        "headSha": lambda x: x.get("head_sha"),
        "workflowName": lambda x: x.get("name"),
        "displayTitle": lambda x: x.get("display_title") or x.get("name"),
    }
    if field in mapping:
        return mapping[field](item)
    snake = re.sub(r"([A-Z])", r"_\1", field).lower()
    return item.get(snake)


def format_json_fields(obj, fields_csv):
    fields = [f.strip() for f in fields_csv.split(",") if f.strip()]
    if isinstance(obj, list):
        return [{f: project_gh_field(it, f) for f in fields} for it in obj]
    if isinstance(obj, dict):
        return {f: project_gh_field(obj, f) for f in fields}
    return obj


def emit_api_error(resp):
    sys.stderr.write(f"gh: GitHub API returned HTTP {resp['statusCode']}\n")
    body = resp.get("body", "")
    if body:
        sys.stderr.write(body.rstrip("\n") + "\n")
    sys.exit(1)


def next_link_path(headers):
    for h in headers or []:
        k, _, v = h.partition(":")
        if k.strip().lower() != "link":
            continue
        for part in v.split(","):
            if 'rel="next"' in part:
                m = re.search(r"<([^>]+)>", part)
                if m:
                    u = urllib.parse.urlparse(m.group(1))
                    return u.path + (f"?{u.query}" if u.query else "")
    return ""


def cmd_api(args):
    method = ""
    headers = []
    fields = []
    input_file = ""
    include = False
    jq_expr = ""
    host = ""
    repo_flag = ""
    silent = False
    paginate = False
    endpoint = ""

    i = 0
    while i < len(args):
        a = args[i]
        if a in ("-X", "--method") and i + 1 < len(args):
            method = args[i + 1].upper()
            i += 2
        elif a.startswith("--method="):
            method = a.split("=", 1)[1].upper()
            i += 1
        elif a in ("-H", "--header") and i + 1 < len(args):
            headers.append(args[i + 1])
            i += 2
        elif a in ("-f", "--raw-field") and i + 1 < len(args):
            k, _, v = args[i + 1].partition("=")
            fields.append((k, v, False))
            i += 2
        elif a in ("-F", "--field") and i + 1 < len(args):
            k, _, v = args[i + 1].partition("=")
            fields.append((k, v, True))
            i += 2
        elif a == "--input" and i + 1 < len(args):
            input_file = args[i + 1]
            i += 2
        elif a in ("-i", "--include"):
            include = True
            i += 1
        elif a in ("-q", "--jq") and i + 1 < len(args):
            jq_expr = args[i + 1]
            i += 2
        elif a.startswith("--jq="):
            jq_expr = a.split("=", 1)[1]
            i += 1
        elif a == "--hostname" and i + 1 < len(args):
            host = args[i + 1]
            i += 2
        elif a in ("-R", "--repo") and i + 1 < len(args):
            repo_flag = args[i + 1]
            i += 2
        elif a == "--silent":
            silent = True
            i += 1
        elif a == "--paginate":
            paginate = True
            i += 1
        elif a in ("-h", "--help"):
            print("Usage: gh api <endpoint> [-X METHOD] [-H HEADER] [-f key=val] [-F key=val] [--input FILE] [-i] [--jq EXPR] [--paginate]")
            return
        elif not a.startswith("-") and not endpoint:
            endpoint = a
            i += 1
        else:
            i += 1

    if not endpoint:
        sys.stderr.write("gh api: endpoint is required (e.g. /user, repos/{owner}/{repo}/pulls, or graphql)\n")
        sys.exit(2)

    det_host, det_repo = detect_repo(repo_flag)
    if not host:
        host = det_host or "github.com"

    if det_repo and "/" in det_repo:
        owner, rname = det_repo.split("/", 1)
        endpoint = endpoint.replace("{owner}", owner).replace("{repo}", rname)
        endpoint = endpoint.replace(":owner", owner).replace(":repo", rname)
    if "{branch}" in endpoint:
        endpoint = endpoint.replace("{branch}", detect_branch())
    if not endpoint.startswith("/"):
        endpoint = "/" + endpoint

    is_graphql = endpoint.split("?", 1)[0] == "/graphql"
    if not method:
        if is_graphql or input_file or fields:
            method = "POST"
        else:
            method = "GET"

    body = ""
    if input_file:
        if input_file == "-":
            body = sys.stdin.read()
        else:
            with open(input_file, "r", encoding="utf-8") as f:
                body = f.read()
    elif fields:
        if method == "GET" and not is_graphql:
            params = []
            for k, v, typed in fields:
                val = parse_typed_value(v) if typed else v
                params.append((k, str(val)))
            sep = "&" if "?" in endpoint else "?"
            endpoint = endpoint + sep + urllib.parse.urlencode(params)
        elif is_graphql:
            payload = {}
            variables = {}
            for k, v, typed in fields:
                val = parse_typed_value(v) if typed else v
                if k == "query":
                    payload["query"] = val
                elif k == "operationName":
                    payload["operationName"] = val
                else:
                    variables[k] = val
            if variables:
                payload["variables"] = variables
            body = json.dumps(payload)
        else:
            payload = {}
            for k, v, typed in fields:
                payload[k] = parse_typed_value(v) if typed else v
            body = json.dumps(payload)

    cur_path = endpoint
    while True:
        resp = proxy_api(method, cur_path, host=host, repo=det_repo, headers=headers, body=body)
        if include and not silent:
            sys.stdout.write(f"HTTP/1.1 {resp['statusCode']}\n")
            for h in resp.get("headers") or []:
                sys.stdout.write(h + "\n")
            sys.stdout.write("\n")
        if resp["statusCode"] >= 400:
            if not silent and resp.get("body"):
                sys.stdout.write(resp["body"])
                if not resp["body"].endswith("\n"):
                    sys.stdout.write("\n")
            sys.stderr.write(f"gh: HTTP {resp['statusCode']}\n")
            sys.exit(1)
        out = resp.get("body", "")
        if not silent:
            if jq_expr:
                sys.stdout.write(run_jq(out, jq_expr))
            else:
                sys.stdout.write(out)
                if out and not out.endswith("\n"):
                    sys.stdout.write("\n")
        if not paginate or method != "GET":
            break
        nxt = next_link_path(resp.get("headers"))
        if not nxt:
            break
        cur_path = nxt


def cmd_auth(args):
    if not args or args[0] in ("-h", "--help"):
        print("Usage: gh auth status [--hostname HOST] [--json]")
        return
    sub = args[0]
    rest = args[1:]
    if sub == "token":
        sys.stderr.write(
            "hotbed-git-auth: raw GitHub tokens stay on the hotbed controller and are not exposed by 'gh auth token'.\n"
            "Use 'gh api ...', 'gh pr ...', 'gh issue ...', or 'git' directly — hotbed delegates auth automatically.\n"
        )
        sys.exit(1)
    if sub == "login":
        sys.stderr.write("hotbed-git-auth: authenticate once on the controller with 'hotbed auth login'.\n")
        sys.exit(1)
    if sub != "status":
        sys.stderr.write(f"gh auth {sub}: unsupported on worker proxy (use 'gh auth status')\n")
        sys.exit(1)

    host = ""
    as_json = False
    i = 0
    while i < len(rest):
        if rest[i] == "--hostname" and i + 1 < len(rest):
            host = rest[i + 1]
            i += 2
        elif rest[i] == "--json":
            as_json = True
            i += 1
        else:
            i += 1

    path = "/gh-auth-status"
    if host:
        path += "?" + urllib.parse.urlencode({"host": host})
    data = socket_json("GET", path)
    creds = data.get("credentials") or []
    if as_json:
        print(json.dumps(data, indent=2))
        return
    if not creds:
        sys.stderr.write("Not logged in to any GitHub hosts on the hotbed controller. Run 'hotbed auth login'.\n")
        sys.exit(1)
    for c in creds:
        user = c.get("username") or c.get("name")
        repos = ",".join(c.get("repos") or []) or "*"
        kind = "OAuth (auto-refresh)" if c.get("refreshable") else "token"
        print(f"{c.get('host', 'github.com')}")
        print(f"  ✓ Logged in to {c.get('host', 'github.com')} account {user} (via hotbed controller credential '{c.get('name')}', {kind}, repos={repos})")


def require_repo(repo_flag):
    host, repo = detect_repo(repo_flag)
    if not repo:
        sys.stderr.write("gh: could not determine repository; pass -R owner/repo or run inside a git checkout\n")
        sys.exit(2)
    return host, repo


def emit_obj(obj, json_fields="", jq_expr="", fallback=None):
    if json_fields:
        out = json.dumps(format_json_fields(obj, json_fields))
        if jq_expr:
            sys.stdout.write(run_jq(out, jq_expr))
        else:
            print(out)
        return
    if jq_expr:
        sys.stdout.write(run_jq(json.dumps(obj), jq_expr))
        return
    if fallback:
        fallback(obj)
    else:
        print(json.dumps(obj, indent=2))


def cmd_repo(args):
    if not args or args[0] != "view":
        sys.stderr.write("gh repo: only 'gh repo view [OWNER/REPO]' is built in; use 'gh api' for other operations\n")
        sys.exit(1)
    rest = args[1:]
    repo_arg = ""
    json_fields = ""
    jq_expr = ""
    i = 0
    while i < len(rest):
        a = rest[i]
        if a == "--json" and i + 1 < len(rest):
            json_fields = rest[i + 1]
            i += 2
        elif a in ("-q", "--jq") and i + 1 < len(rest):
            jq_expr = rest[i + 1]
            i += 2
        elif a in ("-R", "--repo") and i + 1 < len(rest):
            repo_arg = rest[i + 1]
            i += 2
        elif not a.startswith("-") and not repo_arg:
            repo_arg = a
            i += 1
        else:
            i += 1
    host, repo = require_repo(repo_arg)
    resp = proxy_api("GET", f"/repos/{repo}", host=host, repo=repo)
    if resp["statusCode"] >= 400:
        emit_api_error(resp)
    obj = json.loads(resp["body"])
    def show(r):
        print(f"name:\t{r.get('full_name', repo)}")
        print(f"description:\t{r.get('description') or ''}")
        print(f"visibility:\t{'private' if r.get('private') else 'public'}")
        print(f"default_branch:\t{r.get('default_branch', '')}")
        print(f"url:\t{r.get('html_url', '')}")
    emit_obj(obj, json_fields, jq_expr, show)


def read_body_arg(body, body_file):
    if body_file:
        if body_file == "-":
            return sys.stdin.read()
        with open(body_file, "r", encoding="utf-8") as f:
            return f.read()
    return body


def cmd_pr(args):
    if not args or args[0] in ("-h", "--help"):
        print("Usage: gh pr <list|view|create|comment|diff|checks> [flags]")
        return
    sub = args[0]
    rest = args[1:]
    repo_flag = ""
    state = "open"
    limit = "30"
    head = ""
    base = ""
    title = ""
    body = ""
    body_file = ""
    draft = False
    json_fields = ""
    jq_expr = ""
    pos = []
    i = 0
    while i < len(rest):
        a = rest[i]
        if a in ("-R", "--repo") and i + 1 < len(rest):
            repo_flag = rest[i + 1]; i += 2
        elif a in ("-s", "--state") and i + 1 < len(rest):
            state = rest[i + 1]; i += 2
        elif a in ("-L", "--limit") and i + 1 < len(rest):
            limit = rest[i + 1]; i += 2
        elif a in ("-H", "--head") and i + 1 < len(rest):
            head = rest[i + 1]; i += 2
        elif a in ("-B", "--base") and i + 1 < len(rest):
            base = rest[i + 1]; i += 2
        elif a in ("-t", "--title") and i + 1 < len(rest):
            title = rest[i + 1]; i += 2
        elif a in ("-b", "--body") and i + 1 < len(rest):
            body = rest[i + 1]; i += 2
        elif a in ("-F", "--body-file") and i + 1 < len(rest):
            body_file = rest[i + 1]; i += 2
        elif a in ("-d", "--draft"):
            draft = True; i += 1
        elif a == "--json" and i + 1 < len(rest):
            json_fields = rest[i + 1]; i += 2
        elif a in ("-q", "--jq") and i + 1 < len(rest):
            jq_expr = rest[i + 1]; i += 2
        elif not a.startswith("-"):
            pos.append(a); i += 1
        else:
            i += 1

    host, repo = require_repo(repo_flag)
    if sub == "list":
        params = {"state": state, "per_page": limit}
        if head:
            params["head"] = head
        if base:
            params["base"] = base
        resp = proxy_api("GET", f"/repos/{repo}/pulls?{urllib.parse.urlencode(params)}", host=host, repo=repo)
        if resp["statusCode"] >= 400:
            emit_api_error(resp)
        items = json.loads(resp["body"])
        def show(prs):
            for pr in prs:
                print(f"#{pr.get('number')}\t{pr.get('title', '')}\t{(pr.get('head') or {}).get('ref', '')}\t{pr.get('state', '').upper()}")
        emit_obj(items, json_fields, jq_expr, show)
    elif sub == "view":
        if not pos:
            sys.stderr.write("gh pr view: PR number is required\n")
            sys.exit(2)
        num = pos[0].lstrip("#")
        resp = proxy_api("GET", f"/repos/{repo}/pulls/{num}", host=host, repo=repo)
        if resp["statusCode"] >= 400:
            emit_api_error(resp)
        pr = json.loads(resp["body"])
        def show(p):
            print(f"title:\t{p.get('title', '')}")
            print(f"state:\t{p.get('state', '').upper()}")
            print(f"author:\t{(p.get('user') or {}).get('login', '')}")
            print(f"head:\t{(p.get('head') or {}).get('ref', '')}")
            print(f"base:\t{(p.get('base') or {}).get('ref', '')}")
            print(f"url:\t{p.get('html_url', '')}")
            if p.get("body"):
                print("--\n" + p["body"])
        emit_obj(pr, json_fields, jq_expr, show)
    elif sub == "diff":
        if not pos:
            sys.stderr.write("gh pr diff: PR number is required\n")
            sys.exit(2)
        num = pos[0].lstrip("#")
        resp = proxy_api("GET", f"/repos/{repo}/pulls/{num}", host=host, repo=repo, headers=["Accept: application/vnd.github.v3.diff"])
        if resp["statusCode"] >= 400:
            emit_api_error(resp)
        sys.stdout.write(resp["body"])
    elif sub == "checks":
        if not pos:
            sys.stderr.write("gh pr checks: PR number is required\n")
            sys.exit(2)
        num = pos[0].lstrip("#")
        pr_resp = proxy_api("GET", f"/repos/{repo}/pulls/{num}", host=host, repo=repo)
        if pr_resp["statusCode"] >= 400:
            emit_api_error(pr_resp)
        sha = (json.loads(pr_resp["body"]).get("head") or {}).get("sha", "")
        chk_resp = proxy_api("GET", f"/repos/{repo}/commits/{sha}/check-runs", host=host, repo=repo)
        if chk_resp["statusCode"] >= 400:
            emit_api_error(chk_resp)
        runs = json.loads(chk_resp["body"]).get("check_runs") or []
        def show(rs):
            for r in rs:
                print(f"{r.get('name', '')}\t{r.get('status', '')}\t{r.get('conclusion') or '-'}\t{r.get('html_url', '')}")
        emit_obj(runs, json_fields, jq_expr, show)
    elif sub == "create":
        if not head:
            head = detect_branch()
        if not base:
            r_resp = proxy_api("GET", f"/repos/{repo}", host=host, repo=repo)
            if r_resp["statusCode"] < 400:
                base = json.loads(r_resp["body"]).get("default_branch", "main")
            else:
                base = "main"
        payload = {"title": title, "body": read_body_arg(body, body_file), "head": head, "base": base, "draft": draft}
        resp = proxy_api("POST", f"/repos/{repo}/pulls", host=host, repo=repo, body=json.dumps(payload))
        if resp["statusCode"] >= 400:
            emit_api_error(resp)
        pr = json.loads(resp["body"])
        print(pr.get("html_url", ""))
    elif sub == "comment":
        if not pos:
            sys.stderr.write("gh pr comment: PR number is required\n")
            sys.exit(2)
        num = pos[0].lstrip("#")
        payload = {"body": read_body_arg(body, body_file)}
        resp = proxy_api("POST", f"/repos/{repo}/issues/{num}/comments", host=host, repo=repo, body=json.dumps(payload))
        if resp["statusCode"] >= 400:
            emit_api_error(resp)
        print(json.loads(resp["body"]).get("html_url", ""))
    else:
        sys.stderr.write(f"gh pr {sub}: unsupported on worker proxy; use 'gh api' directly\n")
        sys.exit(1)


def cmd_issue(args):
    if not args or args[0] in ("-h", "--help"):
        print("Usage: gh issue <list|view|create|comment> [flags]")
        return
    sub = args[0]
    rest = args[1:]
    repo_flag = ""
    state = "open"
    limit = "30"
    labels = []
    title = ""
    body = ""
    body_file = ""
    json_fields = ""
    jq_expr = ""
    pos = []
    i = 0
    while i < len(rest):
        a = rest[i]
        if a in ("-R", "--repo") and i + 1 < len(rest):
            repo_flag = rest[i + 1]; i += 2
        elif a in ("-s", "--state") and i + 1 < len(rest):
            state = rest[i + 1]; i += 2
        elif a in ("-L", "--limit") and i + 1 < len(rest):
            limit = rest[i + 1]; i += 2
        elif a in ("-l", "--label") and i + 1 < len(rest):
            labels.append(rest[i + 1]); i += 2
        elif a in ("-t", "--title") and i + 1 < len(rest):
            title = rest[i + 1]; i += 2
        elif a in ("-b", "--body") and i + 1 < len(rest):
            body = rest[i + 1]; i += 2
        elif a in ("-F", "--body-file") and i + 1 < len(rest):
            body_file = rest[i + 1]; i += 2
        elif a == "--json" and i + 1 < len(rest):
            json_fields = rest[i + 1]; i += 2
        elif a in ("-q", "--jq") and i + 1 < len(rest):
            jq_expr = rest[i + 1]; i += 2
        elif not a.startswith("-"):
            pos.append(a); i += 1
        else:
            i += 1

    host, repo = require_repo(repo_flag)
    if sub == "list":
        params = {"state": state, "per_page": limit}
        if labels:
            params["labels"] = ",".join(labels)
        resp = proxy_api("GET", f"/repos/{repo}/issues?{urllib.parse.urlencode(params)}", host=host, repo=repo)
        if resp["statusCode"] >= 400:
            emit_api_error(resp)
        items = [x for x in json.loads(resp["body"]) if "pull_request" not in x]
        def show(issues):
            for it in issues:
                print(f"#{it.get('number')}\t{it.get('state', '').upper()}\t{it.get('title', '')}")
        emit_obj(items, json_fields, jq_expr, show)
    elif sub == "view":
        if not pos:
            sys.stderr.write("gh issue view: issue number is required\n")
            sys.exit(2)
        num = pos[0].lstrip("#")
        resp = proxy_api("GET", f"/repos/{repo}/issues/{num}", host=host, repo=repo)
        if resp["statusCode"] >= 400:
            emit_api_error(resp)
        it = json.loads(resp["body"])
        def show(x):
            print(f"title:\t{x.get('title', '')}")
            print(f"state:\t{x.get('state', '').upper()}")
            print(f"author:\t{(x.get('user') or {}).get('login', '')}")
            print(f"url:\t{x.get('html_url', '')}")
            if x.get("body"):
                print("--\n" + x["body"])
        emit_obj(it, json_fields, jq_expr, show)
    elif sub == "create":
        payload = {"title": title, "body": read_body_arg(body, body_file)}
        if labels:
            payload["labels"] = labels
        resp = proxy_api("POST", f"/repos/{repo}/issues", host=host, repo=repo, body=json.dumps(payload))
        if resp["statusCode"] >= 400:
            emit_api_error(resp)
        print(json.loads(resp["body"]).get("html_url", ""))
    elif sub == "comment":
        if not pos:
            sys.stderr.write("gh issue comment: issue number is required\n")
            sys.exit(2)
        num = pos[0].lstrip("#")
        payload = {"body": read_body_arg(body, body_file)}
        resp = proxy_api("POST", f"/repos/{repo}/issues/{num}/comments", host=host, repo=repo, body=json.dumps(payload))
        if resp["statusCode"] >= 400:
            emit_api_error(resp)
        print(json.loads(resp["body"]).get("html_url", ""))
    else:
        sys.stderr.write(f"gh issue {sub}: unsupported on worker proxy; use 'gh api' directly\n")
        sys.exit(1)


def cmd_run(args):
    if not args or args[0] in ("-h", "--help"):
        print("Usage: gh run <list|view> [flags]")
        return
    sub = args[0]
    rest = args[1:]
    repo_flag = ""
    limit = "20"
    branch = ""
    json_fields = ""
    jq_expr = ""
    pos = []
    i = 0
    while i < len(rest):
        a = rest[i]
        if a in ("-R", "--repo") and i + 1 < len(rest):
            repo_flag = rest[i + 1]; i += 2
        elif a in ("-L", "--limit") and i + 1 < len(rest):
            limit = rest[i + 1]; i += 2
        elif a in ("-b", "--branch") and i + 1 < len(rest):
            branch = rest[i + 1]; i += 2
        elif a == "--json" and i + 1 < len(rest):
            json_fields = rest[i + 1]; i += 2
        elif a in ("-q", "--jq") and i + 1 < len(rest):
            jq_expr = rest[i + 1]; i += 2
        elif not a.startswith("-"):
            pos.append(a); i += 1
        else:
            i += 1
    host, repo = require_repo(repo_flag)
    if sub == "list":
        params = {"per_page": limit}
        if branch:
            params["branch"] = branch
        resp = proxy_api("GET", f"/repos/{repo}/actions/runs?{urllib.parse.urlencode(params)}", host=host, repo=repo)
        if resp["statusCode"] >= 400:
            emit_api_error(resp)
        runs = json.loads(resp["body"]).get("workflow_runs") or []
        def show(rs):
            for r in rs:
                print(f"{r.get('id')}\t{r.get('status', '')}\t{r.get('conclusion') or '-'}\t{r.get('name', '')}\t{r.get('head_branch', '')}")
        emit_obj(runs, json_fields, jq_expr, show)
    elif sub == "view":
        if not pos:
            sys.stderr.write("gh run view: run ID is required\n")
            sys.exit(2)
        resp = proxy_api("GET", f"/repos/{repo}/actions/runs/{pos[0]}", host=host, repo=repo)
        if resp["statusCode"] >= 400:
            emit_api_error(resp)
        r = json.loads(resp["body"])
        def show(x):
            print(f"id:\t{x.get('id')}")
            print(f"workflow:\t{x.get('name', '')}")
            print(f"status:\t{x.get('status', '')}")
            print(f"conclusion:\t{x.get('conclusion') or '-'}")
            print(f"url:\t{x.get('html_url', '')}")
        emit_obj(r, json_fields, jq_expr, show)
    else:
        sys.stderr.write(f"gh run {sub}: unsupported on worker proxy; use 'gh api' directly\n")
        sys.exit(1)


def main():
    args = sys.argv[1:]
    if not args or args[0] in ("-h", "--help"):
        print("gh (hotbed controller proxy)")
        print("Commands: api, auth status, repo view, pr, issue, run, version")
        print("All GitHub REST and GraphQL calls are authenticated centrally by the hotbed controller.")
        return
    cmd = args[0]
    rest = args[1:]
    if cmd in ("--version", "version"):
        print("gh version hotbed-proxy (delegated via hotbed controller)")
    elif cmd == "api":
        cmd_api(rest)
    elif cmd == "auth":
        cmd_auth(rest)
    elif cmd == "repo":
        cmd_repo(rest)
    elif cmd == "pr":
        cmd_pr(rest)
    elif cmd == "issue":
        cmd_issue(rest)
    elif cmd == "run":
        cmd_run(rest)
    else:
        sys.stderr.write(f"gh {cmd}: not implemented in hotbed's worker proxy; use 'gh api' for any REST or GraphQL endpoint\n")
        sys.exit(1)


if __name__ == "__main__":
    main()
