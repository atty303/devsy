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
            if user == 'local-user' and (ROOT / 'revoke-local').exists():
                user = None
            forbidden = user == 'local-user' and (ROOT / 'forbid-local').exists() and self.path.startswith(('/repos/', '/api/v3/repos/'))
            body = {'login': user, 'id': 123, 'data': {'viewer': {'login': user}}} if user else {'message': 'Bad credentials'}
            if self.path == '/login/device/code':
                user = 'host-user'
                body = {'device_code': 'fixture-device', 'user_code': 'fixture-code', 'verification_uri': 'https://github.com/login/device', 'expires_in': 600, 'interval': 1}
            elif self.path == '/login/oauth/access_token':
                user = 'host-user'
                body = {'access_token': secrets[HOSTS[0]]['refresh'], 'token_type': 'bearer', 'scope': 'repo,read:org,gist'}
            data = json.dumps(body).encode()
            self.send_response(403 if forbidden else 200 if user else 401)
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
        return subprocess.run(['/fixture/ssh-client', *args], input=token, text=True, capture_output=True, env=e, timeout=20)
    def api(host, expected='host-user', **kw):
        r = gh('api', 'user', '--hostname', host, '--jq', '.login', **kw)
        ok = r.returncode == 0 and r.stdout.strip() == expected
        return ok
    verify('SSH server predates wrapper installation', (ROOT / 'pre-install-gh').read_text().strip() == '/usr/local/bin/gh')
    verify('standard wrapper installed', Path('/usr/local/bin/gh').read_text().startswith('#!/bin/sh\n# devsy gh relay\n'))
    verify('real gh preserved in fixed backup', Path('/usr/local/bin/gh.devsy-original').read_bytes() == (ROOT / 'gh').read_bytes())
    verify('no dedicated profile installed', not Path('/etc/profile.d/devsy-gh.sh').exists())
    host_before = (ROOT / 'host-gh' / 'hosts.yml').read_bytes()
    original = ROOT / 'guest-gh'
    original.mkdir(exist_ok=True)
    def saved():
        return (original / 'hosts.yml').read_text() if (original / 'hosts.yml').exists() else ''
    def no_forwarded_saved():
        return all(v['forwarded'] not in saved() for v in secrets.values())
    def last_diagnostic():
        folder = ROOT / 'guest-cache' / 'devsy' / 'gh-relay' / 'diagnostics'
        return json.loads(max(folder.glob('*.json'), key=lambda f: f.stat().st_mtime_ns).read_text())


    # Dotfiles contain account preferences, never authentication.
    registrations = (
        'github.com:\n    git_protocol: https\n    user: local-user\n    users:\n        local-user:\n'
        'enterprise.one.test:\n    git_protocol: https\n    user: host-user\n    oauth_token: \"\"\n    users:\n        host-user:\n            oauth_token: \"\"\n'
        'enterprise.two.test:\n    git_protocol: https\n    user: dotfiles-user\n    oauth_token:\n    users:\n        dotfiles-user:\n            oauth_token:\n        other-user:\n'
    )
    (original / 'hosts.yml').write_text(registrations)

    # A third-party dispatcher can return to Devsy after resolving the real gh.
    # Exercise both entries without depending on any particular tool manager.
    dispatcher = ROOT / 'dispatcher'
    dispatcher.mkdir()
    shim = dispatcher / 'gh'
    shim.write_text('#!/bin/sh\nexport PATH=/usr/bin:/usr/local/bin:/bin\nif [ -n "$GH_TEST_CONFIG_OVERRIDE" ]; then export GH_CONFIG_DIR="$GH_TEST_CONFIG_OVERRIDE"; fi\nexec /usr/local/bin/gh "$@"\n')
    shim.chmod(0o755)
    def nested_gh(*args, token=None, env=None):
        e = dict(os.environ, PATH=str(dispatcher) + ':/usr/local/bin:/usr/bin:/bin')
        if env:
            e.update(env)
        return subprocess.run(['/usr/local/bin/gh', *args], input=token, text=True, capture_output=True, env=e, timeout=20)
    def nested_api(host, expected='host-user', **kw):
        r = nested_gh('api', 'user', '--hostname', host, '--jq', '.login', **kw)
        return r.returncode == 0 and r.stdout.strip() == expected
    verify('dispatcher re-entry cannot supply forwarded authentication to local lookup', all(nested_api(h) for h in HOSTS))
    verify('dispatcher re-entry preserves real auth status', nested_gh('auth', 'status').returncode == 0)
    verify('dispatcher re-entry records local-only lookup', all(h['source'] == 'forwarded' and h['token_probe_mode'] == 'local_only' for h in last_diagnostic()['hosts']))
    redirected = ROOT / 'redirected-gh'
    redirected.mkdir()
    (redirected / 'hosts.yml').write_text(registrations)
    verify('local-only lookup survives dispatcher config changes', nested_api(HOSTS[0], env={'GH_TEST_CONFIG_OVERRIDE': str(redirected)}))
    verify('nested lookup leaves original preferences unchanged', saved() == registrations and no_forwarded_saved())
    verify('nested lookup retains explicit invalid environment authentication', not nested_api(HOSTS[0], env={'GH_TOKEN': secrets[HOSTS[0]]['invalid']}))
    r = nested_gh('auth', 'login', '--hostname', HOSTS[0], '--with-token', '--insecure-storage', token=secrets[HOSTS[0]]['local'])
    verify('nested local login retains precedence', r.returncode == 0 and nested_api(HOSTS[0], 'local-user'))
    local_nested = saved()
    (original / 'hosts.yml').write_text(local_nested.replace(secrets[HOSTS[0]]['local'], secrets[HOSTS[0]]['invalid']))
    verify('nested invalid local token never falls back', not nested_api(HOSTS[0]))
    (original / 'hosts.yml').write_text(local_nested)
    verify('nested local logout restores forwarding', nested_gh('auth', 'logout', '--hostname', HOSTS[0], '--user', 'local-user').returncode == 0 and nested_api(HOSTS[0]))
    verify('nested alias preparation', gh('alias', 'set', 'probe-guard', '!test -z "$DEVSY_GH_AUTH_PROBE" && gh api user --jq .login').returncode == 0)
    r = nested_gh('probe-guard')
    verify('local-only marker does not leak into aliases or their children', r.returncode == 0 and r.stdout.strip() == 'host-user')
    verify('nested alias removed', gh('alias', 'delete', 'probe-guard').returncode == 0)
    (original / 'hosts.yml').write_text(registrations)

    verify('tokenless registrations use host active accounts', all(api(h) for h in HOSTS))
    verify('non-PTY SSH uses fixed real-gh backup', last_diagnostic()['binary'] == '/usr/local/bin/gh.devsy-original')
    selected = ROOT / 'selected-gh'
    selected.mkdir()
    (selected / 'gh').symlink_to('/usr/bin/gh')
    r = subprocess.run(['/usr/local/bin/gh', 'api', 'user', '--hostname', HOSTS[0], '--jq', '.login'], env=dict(os.environ, PATH=str(selected) + ':/usr/local/bin:/usr/bin:/bin'), text=True, capture_output=True, timeout=20)
    verify('repo-selected gh precedes fixed backup', r.returncode == 0 and r.stdout.strip() == 'host-user' and last_diagnostic()['binary'] == str(selected / 'gh'))
    verify('real auth status ignores hidden tokenless registrations', gh('auth', 'status').returncode == 0)
    verify('unchanged invocation preserves exact dotfiles registrations', saved() == registrations and no_forwarded_saved())
    decisions = last_diagnostic()['hosts']
    verify('registration forwarding retained in diagnostics', decisions and all(h['source'] == 'forwarded' and h.get('account_preferences') and h.get('token_probe') for h in decisions) and decisions[1].get('same_user') and not decisions[2].get('same_user'))
    verify('registration restoration retained in diagnostics', all(r['outcome'] == 'preferences_restored' for r in last_diagnostic()['writeback']['restorations']))
    r = gh('auth', 'logout', '--hostname', HOSTS[2], '--user', 'host-user')
    verify('forwarded logout preserves hidden dotfiles host', r.returncode == 0 and saved() == registrations and api(HOSTS[2]))
    r = gh('config', 'set', 'git_protocol', 'https', '--host', HOSTS[2])
    verify('host setting resave preserves registrations and user preference', r.returncode == 0 and saved() == registrations + '        host-user:\n            git_protocol: https\n')
    verify('forwarding preserves saved per-account settings', gh('config', 'get', 'git_protocol', '--host', HOSTS[2]).stdout.strip() == 'https')
    r = gh('auth', 'login', '--hostname', HOSTS[0], '--with-token', '--insecure-storage', token=secrets[HOSTS[0]]['local'])
    verify('local login from tokenless preferences persists', r.returncode == 0 and api(HOSTS[0], 'local-user') and no_forwarded_saved())
    r = gh('auth', 'logout', '--hostname', HOSTS[0], '--user', 'local-user')
    verify('local logout from preferences restores forwarding', r.returncode == 0 and api(HOSTS[0]))
    (original / 'hosts.yml').write_text(registrations)
    ready_meta, release_meta = ROOT / 'metadata-ready', ROOT / 'metadata-release'
    verify('metadata mutation alias saves', gh('alias', 'set', 'metadata-pending', '!gh config set git_protocol ssh --host enterprise.two.test; touch "$GH_TEST_READY"; while test ! -f "$GH_TEST_RELEASE"; do sleep 0.05; done').returncode == 0)
    e = dict(os.environ, GH_TEST_READY=str(ready_meta), GH_TEST_RELEASE=str(release_meta))
    p = subprocess.Popen(['/bin/bash', '-lc', 'exec gh metadata-pending'], env=e, stdout=subprocess.PIPE, stderr=subprocess.PIPE, start_new_session=True)
    try:
        for _ in range(200):
            if ready_meta.exists():
                break
            time.sleep(0.05)
        verify('tokenless metadata invocation reached mutation', ready_meta.exists())
        changed = registrations.replace('user: dotfiles-user', 'user: parallel-user') + 'unrelated.test:\n    git_protocol: ssh\n'
        (original / 'hosts.yml').write_text(changed)
        release_meta.touch()
        p.communicate(timeout=20)
        verify('host-setting delta preserves parallel user and host changes', p.returncode == 0 and 'user: parallel-user' in saved() and 'unrelated.test:\n    git_protocol: ssh' in saved() and no_forwarded_saved())
        verify('host-setting delta persists at host and account positions', 'enterprise.two.test:\n    git_protocol: ssh' in saved() and '        host-user:\n            git_protocol: ssh' in saved())
    finally:
        if p.poll() is None:
            os.killpg(p.pid, signal.SIGKILL)
            p.communicate()
    verify('metadata alias removed', gh('alias', 'delete', 'metadata-pending').returncode == 0)
    (original / 'hosts.yml').unlink()

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
    verify('existing keyring credential excluded from forwarding', last_diagnostic()['hosts'][1]['source'] == 'keyring')
    verify('nested lookup preserves keyring authentication', nested_api(HOSTS[1], 'local-user') and last_diagnostic()['hosts'][1]['source'] == 'keyring')
    verify('unavailable keyring backend follows gh no-token result', api(HOSTS[1], env={'DBUS_SESSION_BUS_ADDRESS': 'unix:path=/nonexistent'}) and last_diagnostic()['hosts'][1]['source'] == 'forwarded')
    verify('unavailable backend leaves local keyring intact', api(HOSTS[1], 'local-user'))
    revoked = ROOT / 'revoke-local'
    revoked.touch()
    try:
        verify('expired keyring credential never falls back', not api(HOSTS[1]) and last_diagnostic()['hosts'][1]['source'] == 'keyring')
        verify('nested expired keyring credential never falls back', not nested_api(HOSTS[1]) and last_diagnostic()['hosts'][1]['source'] == 'keyring')
    finally:
        revoked.unlink()
    forbidden = ROOT / 'forbid-local'
    forbidden.touch()
    try:
        r = gh('api', 'repos/fixture/private', '--hostname', HOSTS[1])
        verify('insufficient keyring permission never falls back', r.returncode != 0 and '403' in r.stderr and last_diagnostic()['hosts'][1]['source'] == 'keyring')
        r = nested_gh('api', 'repos/fixture/private', '--hostname', HOSTS[1])
        verify('nested insufficient keyring permission never falls back', r.returncode != 0 and '403' in r.stderr and last_diagnostic()['hosts'][1]['source'] == 'keyring')
    finally:
        forbidden.unlink()
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

    verify('host auth unchanged by guest operations', (ROOT / 'host-gh' / 'hosts.yml').read_bytes() == host_before and not (ROOT / 'host-gh' / 'config.yml').exists())

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
