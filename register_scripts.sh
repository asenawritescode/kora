#!/bin/bash
set -e

COOKIE_FILE="/tmp/kora_cookies"
BASE_URL="http://localhost:8000/s/erp.local/api"

# Login
echo "=== Logging in ==="
curl -s -c "$COOKIE_FILE" -X POST "$BASE_URL/auth/login" \
  -H "Content-Type: application/json" \
  -d '{"email":"admin@erp.local","password":"admin123"}' | python3 -m json.tool

CSRF=$(grep kora_csrf "$COOKIE_FILE" | awk '{print $7}')
echo "CSRF: $CSRF"

# Helper: register a script from a file
register_from_file() {
  local name="$1"
  local script_type="$2"
  local doctype="$3"
  local event="$4"
  local file_path="$5"

  echo ""
  echo "=== Registering: $name ==="
  
  SCRIPT_CONTENT=$(python3 -c "import json; print(json.dumps(open('$file_path').read()))")
  
  curl -s -b "$COOKIE_FILE" -X POST "$BASE_URL/system/scripts" \
    -H "Content-Type: application/json" \
    -H "X-Kora-CSRF-Token: $CSRF" \
    -d "{\"name\":$(python3 -c "import json; print(json.dumps('$name'))"),\"script_type\":\"$script_type\",\"doctype\":\"$doctype\",\"event\":\"$event\",\"script\":$SCRIPT_CONTENT}" | python3 -m json.tool
}

# Helper: register inline script
register_inline() {
  local name="$1"
  local script_type="$2"
  local doctype="$3"
  local event="$4"
  local schedule="$5"
  local script_content="$6"

  echo ""
  echo "=== Registering: $name ==="
  
  SCRIPT_JSON=$(python3 -c "import json; print(json.dumps('''$script_content'''))")
  
  curl -s -b "$COOKIE_FILE" -X POST "$BASE_URL/system/scripts" \
    -H "Content-Type: application/json" \
    -H "X-Kora-CSRF-Token: $CSRF" \
    -d "{\"name\":$(python3 -c "import json; print(json.dumps('$name'))"),\"script_type\":\"$script_type\",\"doctype\":\"$doctype\",\"event\":\"$event\",\"schedule\":\"$schedule\",\"script\":$SCRIPT_JSON}" | python3 -m json.tool
}

# 1. Financial Validation
register_from_file "Journal Entry Validation" "doc_event" "Journal Entry" "before_save" "config/v0/erp_kenya/scripts/financial_validation.js"

# 2. Kenya Tax Compliance
register_from_file "Kenya Tax Compliance" "doc_event" "Sales Invoice" "before_save" "config/v0/erp_kenya/scripts/kenya_tax_compliance.js"

# 3. M-Pesa Auto-Matching
register_from_file "M-Pesa Auto-Matching" "doc_event" "M-Pesa Payment" "after_insert" "config/v0/erp_kenya/scripts/mpesa_integration.js"

# 4. eTIMS Invoice Submission
register_from_file "eTIMS Invoice Submission" "doc_event" "Sales Invoice" "on_submit" "config/v0/erp_kenya/scripts/etims_integration.js"

# 5. Kenya Payroll Computation
register_from_file "Kenya Payroll Computation" "doc_event" "Payroll Period" "before_save" "config/v0/erp_kenya/scripts/payroll_kenya.js"

# 6. Stock Validation Rules
register_from_file "Stock Validation Rules" "doc_event" "Stock Movement" "before_save" "config/v0/erp_kenya/scripts/stock_validation.js"

echo ""
echo "=== Done ==="
