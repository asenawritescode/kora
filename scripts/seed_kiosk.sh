#!/bin/bash
# Seed kiosk data for June 1-24 to test analytics
# Usage: bash scripts/seed_kiosk.sh

API="http://localhost:8000/s/kiosk.local/api"
COOKIE_JAR="/tmp/kiosk_cookies.txt"
rm -f "$COOKIE_JAR"

# 1. Login
echo "=== Logging in ==="
curl -s -X POST "$API/auth/login" \
  -c "$COOKIE_JAR" \
  -H 'Content-Type: application/json' \
  -d '{"email":"kiosk@local.dev","password":"kiosk123"}' | python3 -c "import sys,json; d=json.load(sys.stdin); print('Login:', d.get('data',{}).get('email','FAIL'))"

# 2. Get CSRF token (any site API GET)
curl -s "$API/system/doctypes" -b "$COOKIE_JAR" -c "$COOKIE_JAR" > /dev/null
CSRF=$(grep 'kora_csrf' "$COOKIE_JAR" | awk '{print $NF}')

api_call() {
  local method=$1 endpoint=$2 data=$3
  curl -s -X "$method" "$API$endpoint" \
    -b "$COOKIE_JAR" -c "$COOKIE_JAR" \
    -H "Content-Type: application/json" \
    -H "X-Kora-CSRF-Token: $CSRF" \
    -d "$data"
}

# 3. Create 8 products
echo "=== Creating Products ==="
declare -a PRODUCT_IDS
declare -a PRODUCT_NAMES
declare -a PRODUCT_PRICES

create_product() {
  local name=$1 price=$2 cost=$3 cat=$4 sku=$5
  local resp=$(api_call POST /resource/Product \
    "{\"product_name\":\"$name\",\"selling_price\":$price,\"cost_price\":$cost,\"category\":\"$cat\",\"sku\":\"$sku\",\"reorder_level\":5}")
  local pid=$(echo "$resp" | python3 -c "import sys,json; print(json.load(sys.stdin).get('data',{}).get('name','ERR'))" 2>/dev/null)
  echo "  $pid: $name (KSH $price)"
  echo "$pid|$name|$price"
}

while IFS='|' read -r pid pname price; do
  [[ -z "$pid" || "$pid" == "ERR" ]] && continue
  PRODUCT_IDS+=("$pid")
  PRODUCT_NAMES+=("$pname")
  PRODUCT_PRICES+=("$price")
done < <(
  create_product "Soda" 50 25 "Beverages" "SKU001"
  create_product "Chips" 30 15 "Snacks & Sweets" "SKU002"
  create_product "Bottled Water" 20 8 "Beverages" "SKU003"
  create_product "Airtime 100" 100 95 "Airtime & Data" "SKU004"
  create_product "Bread" 40 20 "Household" "SKU005"
  create_product "Soap" 15 7 "Toiletries" "SKU006"
  create_product "Pens" 5 2 "Stationery" "SKU007"
  create_product "Candy" 10 4 "Snacks & Sweets" "SKU008"
)

# 4. Add stock to products
echo "=== Adding Stock ==="
for pid in "${PRODUCT_IDS[@]}"; do
  api_call POST "/resource/Product/$pid" \
    "{\"stock_moves\":[{\"reference\":\"Initial Stock\",\"move_type\":\"Stock In (Purchase)\",\"qty_change\":100,\"unit_cost\":10}]}" > /dev/null
  echo "  Stock added to $pid"
done

# 5. Create daily sales + expenses June 1-24
echo "=== Creating Daily Records (June 1-24) ==="
total_sales=0; total_expenses=0
for day in $(seq 1 24); do
  day_str=$(printf "2026-06-%02d" $day)

  # 3-5 sales per day
  nsales=$(( RANDOM % 3 + 3 ))
  for s in $(seq 1 $nsales); do
    # Pick 1-2 random products
    nitems=$(( RANDOM % 2 + 1 ))
    items="["
    for _ in $(seq 1 $nitems); do
      idx=$(( RANDOM % ${#PRODUCT_IDS[@]} ))
      qty=$(( RANDOM % 3 + 1 ))
      [[ "$items" != "[" ]] && items="$items,"
      items="$items{\"product\":\"${PRODUCT_IDS[$idx]}\",\"quantity\":$qty,\"unit_price\":${PRODUCT_PRICES[$idx]}}"
    done
    items="$items]"

    customers=("John" "Mary" "Peter" "Jane" "Ali" "Grace")
    pmethods=("Cash" "Mobile Money")
    customer=${customers[$((RANDOM % 6))]}
    pmethod=${pmethods[$((RANDOM % 2))]}

    api_call POST /resource/Sale \
      "{\"customer_name\":\"$customer\",\"payment_status\":\"Paid\",\"payment_method\":\"$pmethod\",\"items\":$items}" > /dev/null
    total_sales=$((total_sales + 1))
  done

  # 1-2 expenses per day
  nexp=$(( RANDOM % 2 + 1 ))
  for _ in $(seq 1 $nexp); do
    categories=("Utilities" "Stock Purchase" "Transport" "Supplies" "Staff")
    pmethods=("Cash" "Mobile Money" "Bank Transfer")
    cat=${categories[$((RANDOM % 5))]}
    pm=${pmethods[$((RANDOM % 3))]}
    amount=$(( RANDOM % 1500 + 100 ))

    api_call POST /resource/Expense \
      "{\"description\":\"Daily expense\",\"category\":\"$cat\",\"amount\":$amount,\"date\":\"$day_str\",\"payment_method\":\"$pm\"}" > /dev/null
    total_expenses=$((total_expenses + 1))
  done

  echo "  $day_str: $nsales sales, $nexp expenses"
done

echo "=== Complete ==="
echo "Products: ${#PRODUCT_IDS[@]}"
echo "Sales: $total_sales (over 24 days)"
echo "Expenses: $total_expenses (over 24 days)"
echo "Check analytics at: $API/analytics/status"
