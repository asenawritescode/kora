#!/usr/bin/env python3
"""Register ERP Kenya scripts via Kora API using curl."""
import subprocess, json, os, re, sys
import requests

BASE = "http://localhost:8000/s/erp.local/api"
SCRIPTS_DIR = "config/v0/erp_kenya/scripts"
COOKIE_FILE = "/tmp/kora_cookies"
SESSION = requests.Session()

# Always start fresh
if os.path.exists(COOKIE_FILE):
    os.remove(COOKIE_FILE)

def do_curl(method, path, data=None, include_csrf=True):
    """Execute an authenticated JSON request and return (status_code, response_text)."""
    headers = {"Content-Type": "application/json"}
    if include_csrf and csrf_token:
        headers["X-Kora-CSRF-Token"] = csrf_token
    response = SESSION.request(
        method, f"{BASE}{path}", headers=headers,
        data=data.encode("utf-8") if data else None, timeout=15,
    )
    return response.status_code, response.text

def get_csrf():
    """Extract CSRF token from cookie file."""
    return SESSION.cookies.get("kora_csrf")

# Step 1: Login
print("=== Logging in ===")
csrf_token = None
code, body = do_curl("POST", "/auth/login", 
    json.dumps({"email": "admin@erp.local", "password": "admin123"}), include_csrf=False)
print(f"Status: {code}")
print(body[:200])

# Step 2: Get CSRF token via a GET request
print("\n=== Getting CSRF token ===")
csrf_token = get_csrf()
if not csrf_token:
    # Make a GET request to trigger CSRF cookie set
    code, body = do_curl("GET", "/system/script", include_csrf=False)
    print(f"GET /system/script status: {code}")
    csrf_token = get_csrf()
print(f"CSRF: {csrf_token}")

if not csrf_token:
    print("FATAL: Could not obtain CSRF token")
    sys.exit(1)

# Step 3: Register each script
scripts = [
    ("Journal Entry Validation", "doc_event", "Journal Entry", "before_save", "financial_validation.js"),
    ("Kenya Tax Compliance", "doc_event", "Sales Invoice", "before_save", "kenya_tax_compliance.js"),
    ("M-Pesa Auto-Matching", "doc_event", "M-Pesa Payment", "after_insert", "mpesa_integration.js"),
    # Kora's lifecycle event is named after_submit (not on_submit).
    ("eTIMS Invoice Submission", "doc_event", "Sales Invoice", "after_submit", "etims_integration.js"),
    ("Kenya Payroll Computation", "doc_event", "Payroll Period", "before_save", "payroll_kenya.js"),
    ("Stock Validation Rules", "doc_event", "Stock Movement", "before_save", "stock_validation.js"),
]

for name, stype, doctype, event, filename in scripts:
    print(f"\n=== Registering: {name} ===")
    with open(os.path.join(SCRIPTS_DIR, filename)) as f:
        content = f.read()
    payload = json.dumps({
        "name": name,
        "script_type": stype,
        "doctype": doctype,
        "event": event,
        "script": content
    })
    code, body = do_curl("POST", "/system/script", payload)
    print(f"  Status: {code}")
    try:
        resp = json.loads(body)
        print(f"  Response: {json.dumps(resp, indent=2)[:400]}")
    except:
        print(f"  Response: {body[:300]}")

# Step 4: Scheduled script
print(f"\n=== Registering: eTIMS Failed Retry ===")
scheduled_script = """// Retry failed eTIMS submissions every 15 minutes
var failed = kora.getList("eTIMS Invoice", {
  filters: [
    {field: "status", op: "=", value: "Failed"},
    {field: "retry_count", op: "<", value: "5"}
  ],
  limit: 10
});

for (var i = 0; i < failed.length; i++) {
  kora.log("Retrying eTIMS submission for " + failed[i].name, "info");
  kora.saveDoc("eTIMS Invoice", {
    name: failed[i].name,
    retry_count: (parseInt(failed[i].retry_count || 0)) + 1,
    last_retry: new Date().toISOString()
  });
}"""
payload = json.dumps({
    "name": "eTIMS Failed Retry",
    "script_type": "scheduled",
    "doctype": "",
    "event": "",
    "schedule": "*/15 * * * *",
    "script": scheduled_script
})
code, body = do_curl("POST", "/system/script", payload)
print(f"  Status: {code}")
try:
    resp = json.loads(body)
    print(f"  Response: {json.dumps(resp, indent=2)[:400]}")
except:
    print(f"  Response: {body[:300]}")

print("\n=== All done ===")
