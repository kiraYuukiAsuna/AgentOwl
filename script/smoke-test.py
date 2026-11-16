#!/usr/bin/env python3
"""Exercise a local development API + running Worker; creates named test records."""
import http.cookiejar, json, os, pathlib, time, urllib.error, urllib.parse, urllib.request, uuid
base = os.environ.get('AGENTOWL_SMOKE_URL', 'http://127.0.0.1:8080').rstrip('/')
assert urllib.parse.urlsplit(base).hostname in ('127.0.0.1', 'localhost', '::1'), 'Smoke test requires a loopback development server'
email = os.environ['AGENTOWL_SMOKE_EMAIL']
password = pathlib.Path(os.environ['AGENTOWL_SMOKE_PASSWORD_FILE']).read_text().strip()
client = urllib.request.build_opener(urllib.request.HTTPCookieProcessor(http.cookiejar.CookieJar()))
csrf = ''
def request(method, path, body=None, expected=200, key=None, with_csrf=True):
    headers = {'Content-Type': 'application/json', 'Idempotency-Key': key or str(uuid.uuid4())}
    if with_csrf: headers['X-CSRF-Token'] = csrf
    req = urllib.request.Request(base + path, data=json.dumps(body).encode() if body is not None else None, headers=headers, method=method)
    try: response = client.open(req, timeout=15)
    except urllib.error.HTTPError as error: response = error
    with response:
        status, content, content_type = response.status, response.read(), response.headers.get("Content-Type", "")
    assert status == expected, f'{method} {path}: expected {expected}, got {status}'
    return json.loads(content) if "json" in content_type else {}
assert request('GET','/auth/config')['mode'] == 'local'
request('GET','/api/v1/me',expected=401)
user=request('POST','/auth/login',{'email':email,'password':password});csrf=user['csrf_token']
request('POST','/api/v1/cases',{'title':'must not be created'},expected=403,with_csrf=False)
key=str(uuid.uuid4());body={'title':'Local smoke: Auth feedback','description':'Synthetic local test record; no user data.'}
case=request('POST','/api/v1/cases',body,201,key)
assert request('POST','/api/v1/cases',body,201,key)['id'] == case['id']
request('POST','/api/v1/cases',{'title':'different'},expected=409,key=key)
case=request('POST',f'/api/v1/cases/{case["id"]}/actions',{'action':'claim','expected_version':case['version']})
assert case['assignee_id'] == user['id']
old_version=case['version']
case=request('POST',f'/api/v1/cases/{case["id"]}/feedback',{'action':'resolved','expected_version':old_version,'reason':'Synthetic feedback verification'})
assert case['status']=='resolved'
request('POST',f'/api/v1/cases/{case["id"]}/feedback',{'action':'unresolved','expected_version':old_version},expected=409)
case=request('POST',f'/api/v1/cases/{case["id"]}/feedback',{'action':'human','expected_version':case['version']})
assert case['reply_mode']=='manual'
knowledge=request('POST','/api/v1/knowledge',{'title':'Local smoke: evidence','body':'Ask for CLI version and redacted errors.','visibility':'internal'},201)
ap=request('POST',f'/api/v1/knowledge/{knowledge["id"]}/submit',{'expected_version':knowledge['version']})
ap=request('POST',f'/api/v1/approvals/{ap["id"]}/decisions',{'decision':'approve','expected_version':ap['version']})
for _ in range(30):
    saved=request('GET',f'/api/v1/knowledge/{knowledge["id"]}')
    if saved['status']=='published':break
    time.sleep(.5)
assert saved['status']=='published', 'Worker did not publish approved knowledge'
request('POST','/internal/runner/authorize',{},expected=404)
request('POST','/auth/logout',{})
request('GET','/api/v1/me',expected=401)
print('PASS: auth, CSRF, idempotency, case claim, versioned feedback, knowledge approval, Worker publication, internal-route isolation, logout')
