"""Isolated real-gh oracle. Never print captured auth output or token values."""
import http.server
import json
import os
from pathlib import Path
import signal
import ssl
import subprocess
import sys
import time

ROOT = Path('/fixture')
HOSTS = ['github.com', 'enterprise.one.test', 'enterprise.two.test']
secrets = json.loads((ROOT / 'accounts.json').read_text())

def server():
    subprocess.run(['openssl', 'req', '-x509', '-newkey', 'rsa:2048', '-nodes', '-keyout', '/tmp/fixture-key.pem', '-out', '/tmp/fixture-cert.pem', '-days', '1', '-subj', '/CN=github.com', '-addext', 'subjectAltName=DNS:github.com,DNS:api.github.com,DNS:enterprise.one.test,DNS:enterprise.two.test'], stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL, check=True)
    tls = ssl.SSLContext(ssl.PROTOCOL_TLS_SERVER)
    tls.load_cert_chain('/tmp/fixture-cert.pem', '/tmp/fixture-key.pem')
    tokens = {}
    for record in secrets.values():
        tokens[record['forwarded']] = 'host-user'
        tokens[record['local']] = 'local-user'
        tokens[record['second']] = 'second-user'
        tokens[record['refresh']] = 'host-user'

    class API(http.server.BaseHTTPRequestHandler):
        def log_message(self, *args):
            pass
        def handle_api(self):
            if self.command == 'POST':
                self.rfile.read(int(self.headers.get('Content-Length', 0)))
            token = self.headers.get('Authorization', '').split(' ')[-1]
            user = tokens.get(token)
            body = {'login': user, 'id': 123, 'data': {'viewer': {'login': user}}} if user else {'message': 'Bad credentials'}
            if self.path == '/login/device/code':
                user = 'host-user'
                body = {'device_code': 'fixture-device', 'user_code': 'fixture-code', 'verification_uri': 'https://github.com/login/device', 'expires_in': 600, 'interval': 1}
            elif self.path == '/login/oauth/access_token':
                user = 'host-user'
                body = {'access_token': secrets[HOSTS[0]]['refresh'], 'token_type': 'bearer', 'scope': 'repo,read:org,gist'}
            data = json.dumps(body).encode()
            self.send_response(200 if user else 401)
            self.send_header('X-OAuth-Scopes', 'repo, read:org, gist')
            self.send_header('Content-Type', 'application/json')
            self.send_header('Content-Length', str(len(data)))
            self.end_headers()
            self.wfile.write(data)
        do_GET = handle_api
        do_POST = handle_api

    class Proxy(http.server.BaseHTTPRequestHandler):
        def log_message(self, *args):
            pass
        def do_CONNECT(self):
            if self.path not in [h + ':443' for h in HOSTS + ['api.github.com']]:
                self.send_error(403)
                return
            self.send_response(200)
            self.end_headers()
            self.wfile.flush()
            try:
                with tls.wrap_socket(self.connection, server_side=True) as conn:
                    API(conn, self.client_address, self.server)
            except (ssl.SSLError, ConnectionError):
                pass
    srv = http.server.ThreadingHTTPServer(('127.0.0.1', 8123), Proxy)
    (ROOT / 'api-ready').touch()
    srv.serve_forever()


def check():
    os.environ.update({
        'HTTPS_PROXY': 'http://127.0.0.1:8123', 'HTTP_PROXY': 'http://127.0.0.1:8123',
        'NO_PROXY': 'localhost,127.0.0.1', 'SSL_CERT_FILE': '/tmp/fixture-cert.pem',
        'GH_CONFIG_DIR': '/fixture/guest-gh', 'XDG_DATA_HOME': '/fixture/guest-data',
        'XDG_CACHE_HOME': '/fixture/guest-cache', 'GH_PROMPT_DISABLED': '1',
        'DBUS_SESSION_BUS_ADDRESS': (ROOT / 'bus').read_text().strip(),
    })
    results = []
    def verify(name, ok):
        results.append({'check': name, 'ok': bool(ok)})
        if not ok:
            print(json.dumps(results), flush=True)
            raise SystemExit(1)
    def gh(*args, token=None, env=None):
        e = dict(os.environ)
        if env:
            e.update(env)
        return subprocess.run(['/bin/bash', '-lc', 'exec gh "$@"', '--', *args], input=token, text=True, capture_output=True, env=e, timeout=20)
    def api(host, expected='host-user', **kw):
        r = gh('api', 'user', '--hostname', host, '--jq', '.login', **kw)
        ok = r.returncode == 0 and r.stdout.strip() == expected
        return ok
    original = ROOT / 'guest-gh'
    original.mkdir(exist_ok=True)
    def saved():
        return (original / 'hosts.yml').read_text() if (original / 'hosts.yml').exists() else ''
    def no_forwarded_saved():
        return all(v['forwarded'] not in saved() for v in secrets.values())
    def last_diagnostic():
        folder = ROOT / 'guest-cache' / 'devsy' / 'gh-relay' / 'diagnostics'
        return json.loads(max(folder.glob('*.json'), key=lambda f: f.stat().st_mtime_ns).read_text())


    verify('all hosts forwarded through real RPC and HTTP relay', all(api(h) for h in HOSTS))
    verify('real auth status', gh('auth', 'status').returncode == 0)
    verify('forwarding never saved', no_forwarded_saved())
    verify('unchanged authentication run recorded without user delta', not last_diagnostic()['writeback'].get('changes'))
    r = gh('api', 'user', '--jq', '.login', env={'GH_HOST': HOSTS[2]})
    verify('gh chooses GH_HOST', r.returncode == 0 and r.stdout.strip() == 'host-user')
    verify('explicit invalid GH_TOKEN never falls back', gh('api', 'user', env={'GH_TOKEN': secrets[HOSTS[0]]['invalid']}).returncode != 0)
    verify('explicit invalid GITHUB_TOKEN never falls back', gh('api', 'user', env={'GITHUB_TOKEN': secrets[HOSTS[0]]['invalid']}).returncode != 0)
    verify('explicit enterprise environment never falls back', gh('api', 'user', '--hostname', HOSTS[1], env={'GH_ENTERPRISE_TOKEN': secrets[HOSTS[1]]['invalid']}).returncode != 0)
    r = gh('auth', 'login', '--hostname', HOSTS[0], '--with-token', '--insecure-storage', token=secrets[HOSTS[0]]['forwarded'])
    verify('same token login stays forwarded', r.returncode == 0 and no_forwarded_saved() and api(HOSTS[0]))
    verify('auth switch uses forwarded account', gh('auth', 'switch', '--hostname', HOSTS[0], '--user', 'host-user').returncode == 0 and no_forwarded_saved())
    r = gh('auth', 'refresh', '--hostname', HOSTS[0], '--insecure-storage', env={'GH_BROWSER': '/bin/true'})
    verify('refresh saves a newly issued credential', r.returncode == 0 and secrets[HOSTS[0]]['refresh'] in saved() and api(HOSTS[0]))
    verify('logout refreshed credential', gh('auth', 'logout', '--hostname', HOSTS[0], '--user', 'host-user').returncode == 0 and api(HOSTS[0]))
    r = gh('auth', 'login', '--hostname', HOSTS[0], '--with-token', '--insecure-storage', token=secrets[HOSTS[0]]['local'])
    verify('different token local login persists', r.returncode == 0 and secrets[HOSTS[0]]['local'] in saved() and api(HOSTS[0], 'local-user'))
    local_text = saved()
    (original / 'hosts.yml').write_text(local_text.replace(secrets[HOSTS[0]]['local'], secrets[HOSTS[0]]['invalid']))
    verify('invalid local credential never falls back', not api(HOSTS[0]))
    (original / 'hosts.yml').write_text(local_text)
    r = gh('auth', 'logout', '--hostname', HOSTS[0], '--user', 'local-user')
    verify('logout restores forwarding on next invocation', r.returncode == 0 and api(HOSTS[0]) and no_forwarded_saved())

    r = gh('auth', 'login', '--hostname', HOSTS[1], '--with-token', token=secrets[HOSTS[1]]['local'])
    verify('keyring login succeeds without plaintext token', r.returncode == 0 and secrets[HOSTS[1]]['local'] not in saved() and api(HOSTS[1], 'local-user'))
    r = gh('auth', 'status', '--hostname', HOSTS[1])
    verify('real auth status identifies keyring', r.returncode == 0 and 'keyring' in r.stdout + r.stderr)
    verify('keyring logout restores forwarding', gh('auth', 'logout', '--hostname', HOSTS[1], '--user', 'local-user').returncode == 0 and api(HOSTS[1]))

    r = gh('auth', 'login', '--hostname', HOSTS[1], '--with-token', token=secrets[HOSTS[1]]['forwarded'])
    verify('same token explicit keyring login retained', r.returncode == 0 and 'host-user' in saved() and no_forwarded_saved() and api(HOSTS[1]))
    verify('same token keyring logout restores forwarding', gh('auth', 'logout', '--hostname', HOSTS[1], '--user', 'host-user').returncode == 0 and api(HOSTS[1]))

    verify('alias saves', gh('alias', 'set', 'save-editor', '!gh config set editor nano').returncode == 0)
    verify('alias nested configuration persists', gh('save-editor').returncode == 0 and gh('config', 'get', 'editor').stdout.strip() == 'nano')
    ext = ROOT / 'guest-data' / 'gh' / 'extensions' / 'gh-probe'
    ext.mkdir(parents=True)
    (ext / 'gh-probe').write_text('#!/bin/sh\ngh config set pager cat\ngh api user --hostname enterprise.two.test --jq .login\nprintf "extension-out\\n"\nprintf "extension-err\\n" >&2\nexit 7\n')
    (ext / 'gh-probe').chmod(0o755)
    r = gh('probe')
    verify('extension streams and failure exit pass unchanged', r.returncode == 7 and r.stdout == 'host-user\nextension-out\n' and r.stderr == 'extension-err\n')
    verify('extension change persists after failure', gh('config', 'get', 'pager').stdout.strip() == 'cat')

    # Symlinked config and dotfiles replacement between snapshot and writeback.
    config = original / 'config.yml'
    target = ROOT / 'dotfiles-config.yml'
    config.rename(target)
    config.symlink_to(target)
    ready, release = ROOT / 'command-ready', ROOT / 'command-release'
    verify('concurrent alias saves', gh('alias', 'set', 'pending-editor', '!gh config set editor pending; touch "$GH_TEST_READY"; while test ! -f "$GH_TEST_RELEASE"; do sleep 0.05; done').returncode == 0)
    def pending():
        e = dict(os.environ, GH_TEST_READY=str(ready), GH_TEST_RELEASE=str(release))
        return subprocess.Popen(['/bin/bash', '-lc', 'exec gh pending-editor'], env=e, stdout=subprocess.PIPE, stderr=subprocess.PIPE, start_new_session=True)
    def wait_ready():
        for _ in range(200):
            if ready.exists():
                return True
            time.sleep(0.05)
        return False
    p = pending()
    try:
        verify('concurrent invocation reached user mutation', wait_ready())
        verify('parallel gh saves unrelated pager', gh('config', 'set', 'pager', 'less').returncode == 0)
        target.write_text(target.read_text().replace('editor: nano', 'editor: emacs') + '\nprompt: disabled\n')
        release.touch()
        p.communicate(timeout=20)
        changes = last_diagnostic()['writeback']['changes']
        verify('successful current-wins merge recorded', changes.get('config.editor', {}).get('current_wins') == 1 and last_diagnostic()['writeback']['written_files'] > 0)
        verify('parallel and dotfiles changes survive', p.returncode == 0 and gh('config', 'get', 'editor').stdout.strip() == 'pending' and gh('config', 'get', 'pager').stdout.strip() == 'less' and gh('config', 'get', 'prompt').stdout.strip() == 'disabled' and config.is_symlink())
    finally:
        if p.poll() is None:
            os.killpg(p.pid, signal.SIGKILL)
            p.communicate()
    ready.unlink()
    release.unlink()
    verify('prepare interrupted mutation', gh('config', 'set', 'editor', 'vi').returncode == 0)
    p = pending()
    try:
        verify('interrupted invocation reached mutation', wait_ready())
    finally:
        os.killpg(p.pid, signal.SIGKILL)
        p.communicate()
    verify('next invocation recovers killed wrapper mutation', gh('config', 'get', 'editor').stdout.strip() == 'pending')
    verify('recovered session contains no forwarded credentials', no_forwarded_saved())

    # A writeback failure must preserve gh's exit status and retain the changes.
    ready.unlink()
    verify('prepare pending writeback', gh('config', 'set', 'editor', 'vi').returncode == 0)
    p = pending()
    latest = None
    try:
        verify('writeback failure reached user mutation', wait_ready())
        latest = target.read_text()
        target.write_text('[broken yaml')
        release.touch()
        p.communicate(timeout=20)
        verify('writeback failure does not change real gh exit', p.returncode == 0)
        diagnostic = last_diagnostic()
        verify('writeback schema failure retained in diagnostics', diagnostic['stage'] == 'writeback_pending' and any(op['name'] == 'config.merge' and op.get('error_type') == 'schema_invalid' for op in diagnostic['operations']))
    finally:
        if latest is not None:
            target.write_text(latest)
        if p.poll() is None:
            os.killpg(p.pid, signal.SIGKILL)
            p.communicate()
    verify('pending writeback recovered after original repaired', gh('config', 'get', 'editor').stdout.strip() == 'pending')


    # Killing only the wrapper must not reap the still-running real gh.
    ready.unlink()
    release.unlink()
    verify('prepare live-child ownership', gh('config', 'set', 'editor', 'vi').returncode == 0)
    p = pending()
    try:
        verify('live child reached mutation', wait_ready())
        os.kill(p.pid, signal.SIGKILL)
        r = gh('config', 'get', 'editor')
        verify('live child prevents premature recovery', r.returncode == 0 and r.stdout.strip() == 'vi' and bool(list((ROOT / 'guest-cache' / 'devsy' / 'gh-relay').glob('session-*'))))
        release.touch()
        p.communicate(timeout=20)
        verify('completed orphan child changes recover', gh('config', 'get', 'editor').stdout.strip() == 'pending')
    finally:
        if p.poll() is None:
            os.killpg(p.pid, signal.SIGKILL)
            p.communicate()

    verify('host auth unchanged by guest operations', all(v['forwarded'] in (ROOT / 'host-gh' / 'hosts.yml').read_text() for v in secrets.values()))

    # A host active-account change must be observed on the very next invocation.
    hp = ROOT / 'host-gh' / 'hosts.yml'

    host_before = hp.read_text()
    try:
        hp.write_text('{}\n')
        r = gh('config', 'get', 'editor')
        first = last_diagnostic()
        hp.write_text('[broken yaml')
        other = gh('config', 'get', 'editor')
        second = last_diagnostic()
        verify('same exit distinguishes host authentication failures', r.returncode == other.returncode == 0 and first['host_status'] == 'no_active_accounts' and second['host_status'] == 'config_decode_failed')
    finally:
        hp.write_text(host_before)
    text = hp.read_text().replace('host-user', 'second-user')
    for v in secrets.values():
        text = text.replace(v['forwarded'], v['second'])
    hp.write_text(text)
    verify('host active account followed', api(HOSTS[2], 'second-user'))
    r = subprocess.run(['runuser', '-u', 'vscode', '--', 'env', 'HOME=/home/vscode', 'GH_CONFIG_DIR=', 'XDG_CONFIG_HOME=', 'XDG_CACHE_HOME=/home/vscode/.cache', '/bin/bash', '-lc', 'exec gh api user --hostname enterprise.two.test --jq .login'], env=dict(os.environ, DBUS_SESSION_BUS_ADDRESS='unix:path=/nonexistent'), text=True, capture_output=True, timeout=20)
    verify('non-root target user inherits wrapper and uses own HOME', r.returncode == 0 and r.stdout.strip() == 'second-user')

    verify('local config dir override', gh('config', 'set', 'editor', 'emacs', env={'GH_CONFIG_DIR': '/fixture/custom-gh'}).returncode == 0 and (ROOT / 'custom-gh' / 'config.yml').exists())
    verify('XDG config location', gh('config', 'set', 'editor', 'emacs', env={'GH_CONFIG_DIR': '', 'XDG_CONFIG_HOME': '/fixture/custom-xdg'}).returncode == 0 and (ROOT / 'custom-xdg' / 'gh' / 'config.yml').exists())
    verify('HOME config location', gh('config', 'set', 'editor', 'emacs', env={'GH_CONFIG_DIR': '', 'XDG_CONFIG_HOME': '', 'HOME': '/fixture/custom-home'}).returncode == 0 and (ROOT / 'custom-home' / '.config' / 'gh' / 'config.yml').exists())
    records = list((ROOT / 'guest-cache' / 'devsy' / 'gh-relay' / 'diagnostics').glob('*.json'))
    verify('bounded local diagnostics', 0 < len(records) <= 64)
    verify('diagnostics contain no credential values', all(all(secret not in f.read_text() for values in secrets.values() for secret in values.values()) for f in records))
    before = {f.name for f in records}
    r = gh('config', 'set', 'browser', '/bin/true', env={'DEVSY_GH_DIAGNOSTICS': 'false'})
    verify('disabled diagnostics preserve configuration writes', r.returncode == 0 and gh('config', 'get', 'browser', env={'DEVSY_GH_DIAGNOSTICS': 'false'}).stdout.strip() == '/bin/true')
    verify('diagnostics opt out', api(HOSTS[2], 'second-user', env={'DEVSY_GH_DIAGNOSTICS': 'false'}) and before == {f.name for f in records[0].parent.glob('*.json')})

    diagnostic_dir = ROOT / 'guest-cache' / 'devsy' / 'gh-relay' / 'diagnostics'
    held = diagnostic_dir.with_name('held-diagnostics')
    diagnostic_dir.rename(held)
    diagnostic_dir.symlink_to('/proc/1')
    try:
        verify('recording failure does not change authentication', api(HOSTS[2], 'second-user'))
    finally:
        diagnostic_dir.unlink()
        held.rename(diagnostic_dir)
    verify('normal sessions cleaned', not list((ROOT / 'guest-cache' / 'devsy' / 'gh-relay').glob('session-*')))

    # Leave one interrupted change for the caller to recover after stopping RPC.
    ready.unlink()
    release.unlink()
    verify('prepare relay-loss recovery', gh('config', 'set', 'editor', 'vi').returncode == 0)
    p = pending()
    try:
        verify('relay-loss mutation reached', wait_ready())
    finally:
        os.killpg(p.pid, signal.SIGKILL)
        p.communicate()
    print(json.dumps(results), flush=True)

if sys.argv[1] == 'server':
    server()
else:
    check()
