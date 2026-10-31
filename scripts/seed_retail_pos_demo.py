#!/usr/bin/env python3
"""Seed two years of deterministic Retail POS demo history through the resource API.

Required environment variables:
  KORA_DEMO_COOKIE  Full Cookie header for an authenticated retail workspace session.
  KORA_DEMO_CSRF    Matching kora_csrf token.
"""

from __future__ import annotations

import concurrent.futures
import hashlib
import json
import os
import random
import threading
import time as time_module
import urllib.error
import urllib.parse
import urllib.request
from datetime import date, datetime, time, timedelta, timezone


BASE_URL = os.environ.get("KORA_DEMO_BASE_URL", "http://localhost:5173/s/retail").rstrip("/")
COOKIE = os.environ.get("KORA_DEMO_COOKIE", "")
CSRF = os.environ.get("KORA_DEMO_CSRF", "")
ADMIN_USER = "01M59GTE25EKFJSJ4N0JBFG0WD"
START_DATE = date(2024, 10, 20)
END_DATE = date(2026, 10, 19)
LOCK = threading.Lock()
COUNTS: dict[str, int] = {}
EXISTING: dict[tuple[str, str], dict[str, dict]] = {}


def request(method: str, path: str, body: dict | None = None) -> dict:
    data = None if body is None else json.dumps(body).encode()
    headers = {"Accept": "application/json", "Content-Type": "application/json", "Cookie": COOKIE}
    if method != "GET":
        headers["X-Kora-CSRF-Token"] = CSRF
    for attempt in range(7):
        req = urllib.request.Request(BASE_URL + path, data=data, method=method, headers=headers)
        try:
            with urllib.request.urlopen(req, timeout=60) as response:
                payload = json.load(response)
            return payload.get("data", payload)
        except urllib.error.HTTPError as error:
            detail = error.read().decode()[:1000]
            if error.code in (429, 502, 503) and attempt < 6:
                retry_after = error.headers.get("Retry-After")
                delay = float(retry_after) if retry_after and retry_after.isdigit() else min(0.5 * (2**attempt), 8)
                time_module.sleep(delay + random.random() * 0.2)
                continue
            raise RuntimeError(f"{method} {path} failed with HTTP {error.code}: {detail}") from error
    raise RuntimeError(f"{method} {path} exhausted retries")


def create(doctype: str, record: dict) -> dict:
    result = request("POST", "/api/v1/resource/" + urllib.parse.quote(doctype, safe=""), record)
    with LOCK:
        COUNTS[doctype] = COUNTS.get(doctype, 0) + 1
    return result


def transition_action(doctype: str, name: str, action: str) -> dict:
    # Status changes use the canonical operation kernel command.
    identity = f"retail-pos-seed:{doctype}:{name}:{action}"
    key = hashlib.sha256(identity.encode()).hexdigest()
    return request(
        "POST",
        "/api/v1/kernel/workflow.status_transition",
        {
            "idempotency_key": key,
            "payload": {"doctype": doctype, "name": name, "action": action},
        },
    )


def list_records(doctype: str) -> list[dict]:
    records: list[dict] = []
    offset = 0
    while True:
        result = request(
            "GET",
            "/api/v1/resource/"
            + urllib.parse.quote(doctype, safe="")
            + f"?limit=100&offset={offset}",
        )
        page = result if isinstance(result, list) else []
        if not page:
            return records
        records.extend(page)
        offset += len(page)
        if len(page) < 100:
            return records


def existing_by(doctype: str, field: str) -> dict[str, dict]:
    cache_key = (doctype, field)
    with LOCK:
        cached = EXISTING.get(cache_key)
    if cached is not None:
        return cached
    loaded = {str(row.get(field)): row for row in list_records(doctype) if row.get(field) not in (None, "")}
    with LOCK:
        return EXISTING.setdefault(cache_key, loaded)


def ensure(doctype: str, unique_field: str, records: list[dict]) -> list[dict]:
    existing = existing_by(doctype, unique_field)
    result = []
    for record in records:
        key = str(record[unique_field])
        item = existing.get(key)
        if item is None:
            item = create(doctype, record)
            with LOCK:
                existing[key] = item
        result.append(item)
    return result


def create_unique(doctype: str, unique_field: str, record: dict) -> tuple[dict, bool]:
    existing = existing_by(doctype, unique_field)
    key = str(record[unique_field])
    item = existing.get(key)
    if item is not None:
        return item, False
    item = create(doctype, record)
    with LOCK:
        existing[key] = item
    return item, True


def timestamp(day: date, hour: int, minute: int = 0) -> str:
    return datetime.combine(day, time(hour, minute), tzinfo=timezone.utc).isoformat().replace("+00:00", "Z")


def demo_master_data() -> tuple[list[dict], list[dict], list[dict], dict]:
    categories = ensure(
        "Product Category",
        "category_name",
        [
            {"category_name": "Demo Groceries", "display_order": 1, "is_active": 1, "color": "#16a34a"},
            {"category_name": "Demo Beverages", "display_order": 2, "is_active": 1, "color": "#0284c7"},
            {"category_name": "Demo Household", "display_order": 3, "is_active": 1, "color": "#7c3aed"},
            {"category_name": "Demo Personal Care", "display_order": 4, "is_active": 1, "color": "#db2777"},
            {"category_name": "Demo Bakery", "display_order": 5, "is_active": 1, "color": "#d97706"},
        ],
    )
    category_names = {row["category_name"]: row["name"] for row in categories}
    catalogue = [
        ("Rice 2kg", "Groceries", 420), ("Maize Flour 2kg", "Groceries", 185),
        ("Wheat Flour 2kg", "Groceries", 210), ("Cooking Oil 1L", "Groceries", 360),
        ("Sugar 2kg", "Groceries", 330), ("Salt 1kg", "Groceries", 95),
        ("Black Tea 100 bags", "Beverages", 285), ("Ground Coffee 250g", "Beverages", 620),
        ("Bottled Water 1L", "Beverages", 85), ("Fruit Juice 1L", "Beverages", 245),
        ("Soda 500ml", "Beverages", 90), ("Fresh Milk 500ml", "Beverages", 75),
        ("Laundry Detergent 1kg", "Household", 310), ("Dishwashing Liquid", "Household", 190),
        ("Toilet Tissue 4 pack", "Household", 225), ("Kitchen Towels", "Household", 170),
        ("Bath Soap", "Personal Care", 120), ("Toothpaste 150ml", "Personal Care", 230),
        ("Body Lotion 400ml", "Personal Care", 480), ("Shampoo 400ml", "Personal Care", 520),
        ("White Bread", "Bakery", 75), ("Brown Bread", "Bakery", 85),
        ("Mandazi 4 pack", "Bakery", 120), ("Tea Scones 6 pack", "Bakery", 180),
    ]
    products = ensure(
        "Product",
        "reference",
        [
            {
                "reference": f"DEMO-SKU-{index:03d}",
                "product_name": name,
                "category": category_names[f"Demo {category}"],
                "selling_price": price,
                "stock_qty": 100 + (index * 7) % 180,
                "status": "Active",
            }
            for index, (name, category, price) in enumerate(catalogue, 1)
        ],
    )
    first_names = ["Amina", "Brian", "Carol", "David", "Esther", "Faith", "George", "Hassan", "Irene", "James", "Kevin", "Lilian", "Mary", "Njeri", "Omar", "Peter", "Ruth", "Samuel", "Wanjiku", "Yusuf"]
    last_names = ["Kamau", "Otieno", "Wambui", "Mwangi", "Achieng", "Kiptoo", "Mutua", "Njoroge", "Wafula", "Maina"]
    customers = ensure(
        "POS Customer",
        "phone",
        [
            {
                "customer_name": f"{first_names[i % len(first_names)]} {last_names[(i * 3) % len(last_names)]} Demo {i + 1:03d}",
                "phone": f"+2547009{i + 1:04d}",
                "email": f"demo.customer.{i + 1:03d}@example.test",
                "notes": "Synthetic customer for the two-year Retail POS demo.",
                "status": "Active" if i < 72 else "Inactive",
            }
            for i in range(80)
        ],
    )
    registers = ensure(
        "Register",
        "reference",
        [
            {"reference": "DEMO-REG-01", "register_name": "Demo Front Counter", "location": "Main entrance", "status": "Active", "supervisor": ADMIN_USER},
            {"reference": "DEMO-REG-02", "register_name": "Demo Express Counter", "location": "Express lane", "status": "Active", "supervisor": ADMIN_USER},
        ],
    )
    accounts = ensure(
        "POS Account",
        "account_number",
        [
            {"account_number": "DEMO-EXP-001", "account_name": "Demo Store Operations", "account_type": "Expense", "can_post": 1, "is_active": 1},
            {"account_number": "DEMO-INC-001", "account_name": "Demo Retail Sales", "account_type": "Income", "can_post": 1, "is_active": 1},
        ],
    )
    return products, customers, registers, {row["account_number"]: row for row in accounts}


def seed_day(day: date, products: list[dict], customers: list[dict], registers: list[dict], expense_account: str) -> None:
    rng = random.Random(day.toordinal() * 104729)
    day_key = day.strftime("%Y%m%d")
    register = registers[day.toordinal() % len(registers)]
    opening = 5000 + (day.day % 6) * 500
    shift_ref = f"DEMO-SHIFT-{day_key}"
    shift = ensure(
        "Shift",
        "reference",
        [{
            "reference": shift_ref,
            "status": "Planned",
            "supervisor": ADMIN_USER,
            "started_at": timestamp(day, 7, 30),
            "ended_at": timestamp(day, 20, 0),
            "notes": "Synthetic completed shift for trend and workflow demonstrations.",
        }],
    )[0]
    if shift.get("status") == "Planned":
        shift = transition_action("Shift", shift["name"], "Open shift")
    if shift.get("status") == "Open":
        shift = transition_action("Shift", shift["name"], "Close shift")
    session_ref = f"DEMO-TILL-{day_key}"
    session_existing = existing_by("Till Session", "reference")
    session = session_existing.get(session_ref)
    if session is None:
        session, _ = create_unique(
            "Till Session",
            "reference",
            {
                "reference": session_ref,
                "register": register["name"],
                "shift": shift["name"],
                "opened_by": ADMIN_USER,
                "closed_by": ADMIN_USER,
                "opening_cash": opening,
                "cash_sales": 0,
                "cash_refunds": 0,
                "expected_cash": opening,
                "counted_cash": opening,
                "variance": 0,
                "closed_at": timestamp(day, 20, 0),
                "evidence": "Synthetic till close generated for the two-year demo.",
                "status": "Closed",
            },
        )
    sales_count = 2 + int(day.weekday() >= 4) + int(day.month in (11, 12))
    daily_cash = 0.0
    daily_refunds = 0.0
    for sale_index in range(1, sales_count + 1):
        ref = f"DEMO-SALE-{day_key}-{sale_index:02d}"
        existing_sale = existing_by("Sale", "reference").get(ref)
        customer = customers[rng.randrange(len(customers))]
        selected = rng.sample(products, rng.randint(1, 4))
        items = []
        subtotal = 0.0
        for product in selected:
            quantity = rng.randint(1, 3)
            price = float(product["selling_price"])
            discount = round(price * quantity * (0.05 if rng.random() < 0.10 else 0), 2)
            line_total = round(price * quantity - discount, 2)
            items.append({"product": product["name"], "quantity": quantity, "unit_price": price, "discount_amount": discount, "tax_amount": 0, "line_total": line_total})
            subtotal += line_total
        discount_amount = round(subtotal * (0.03 if rng.random() < 0.08 else 0), 2)
        total = round(subtotal - discount_amount, 2)
        roll = rng.random()
        sale_status = "Cancelled" if roll < 0.015 else "Refunded" if roll < 0.045 else "Closed"
        sale, _ = create_unique(
            "Sale", "reference", {
                "reference": ref,
                "status": sale_status,
                "customer": customer["name"],
                "cashier": ADMIN_USER,
                "register": register["name"],
                "shift": shift["name"],
                "till_session": session["name"],
                "subtotal": round(subtotal, 2),
                "discount_amount": discount_amount,
                "tax_amount": 0,
                "total": total,
                "items": items,
            },
        )
        if sale_status != "Cancelled":
            method = rng.choices(["Cash", "Card", "Mobile Money"], [0.48, 0.18, 0.34])[0]
            payment, payment_created = create_unique(
                "Payment", "reference", {
                    "reference": f"DEMO-PAY-{day_key}-{sale_index:02d}",
                    "source_record": ref,
                    "assignee": ADMIN_USER,
                    "evidence": "Synthetic payment for historical POS reporting.",
                    "status": "Draft",
                    "sale": sale["name"],
                    "register": register["name"],
                    "shift": shift["name"],
                    "till_session": session["name"],
                    "amount": total,
                    "method": method,
                    "provider_reference": f"DEMO-TXN-{day_key}-{sale_index:02d}" if method != "Cash" else "",
                },
            )
            if payment_created:
                payment = transition_action("Payment", payment["name"], "Start STK Push")
                payment = transition_action("Payment", payment["name"], "Await provider")
                payment = transition_action("Payment", payment["name"], "Confirm payment")
                if sale_status == "Refunded":
                    transition_action("Payment", payment["name"], "Finalize payment")
            if method == "Cash":
                daily_cash += total
            if sale_status == "Refunded":
                daily_refunds += total
                create_unique(
                    "Return", "reference", {
                        "reference": f"DEMO-RETURN-{day_key}-{sale_index:02d}",
                        "source_record": sale["name"],
                        "assignee": ADMIN_USER,
                        "evidence": "Synthetic customer return for refund-rate reporting.",
                        "status": "Completed",
                    },
                )
    if day.weekday() == 0:
        product = products[(day.toordinal() // 7) % len(products)]
        create_unique(
            "Stock Movement", "reference", {
                "reference": f"DEMO-STOCK-{day_key}",
                "product": product["name"],
                "movement_type": "Receipt",
                "quantity": 40 + rng.randrange(10, 80),
                "unit_cost": round(float(product["selling_price"]) * 0.68, 2),
                "source_type": "Demo supplier receipt",
                "source_reference": f"DEMO-GRN-{day_key}",
                "reason": "Weekly synthetic replenishment.",
                "posting_date": timestamp(day, 8, 0),
                "status": "Posted",
            },
        )
        expense_amount = 700 + rng.randrange(0, 2300)
        create_unique(
            "POS Expense", "reference", {
                "reference": f"DEMO-EXP-{day_key}",
                "expense_date": day.isoformat(),
                "payee": rng.choice(["Demo Cleaning Services", "Demo Packaging Supplier", "Demo Local Transport", "Demo Store Maintenance"]),
                "amount": expense_amount,
                "account": expense_account,
                "till_session": session["name"],
                "payment_method": rng.choice(["Cash", "Mobile Money", "Bank Transfer"]),
                "reason": "Synthetic weekly operating expense.",
                "status": "Paid",
                "submitted_by": ADMIN_USER,
                "approved_by": ADMIN_USER,
            },
        )
    if day.day == 28:
        cash_movement, cash_created = create_unique(
            "Cash Movement", "reference", {
                "reference": f"DEMO-CASH-{day_key}",
                "till_session": session["name"],
                "movement_type": "Safe Drop",
                "amount": round(max(daily_cash * 0.7, 1000), 2),
                "reason": "Synthetic month-end safe drop.",
                "approved_by": ADMIN_USER,
                "status": "Draft",
            },
        )
        if cash_created:
            cash_movement = transition_action("Cash Movement", cash_movement["name"], "Submit movement")
            transition_action("Cash Movement", cash_movement["name"], "Approve movement")


def main() -> None:
    if not COOKIE or not CSRF:
        raise SystemExit("KORA_DEMO_COOKIE and KORA_DEMO_CSRF are required")
    request("GET", "/api/auth/me")
    for doctype, field in [
        ("Product Category", "category_name"), ("Product", "reference"),
        ("POS Customer", "phone"), ("Register", "reference"),
        ("POS Account", "account_number"), ("Inventory Balance", "product"),
        ("Shift", "reference"), ("Till Session", "reference"),
        ("Sale", "reference"), ("Payment", "reference"),
        ("Return", "reference"), ("Stock Movement", "reference"),
        ("POS Expense", "reference"), ("Cash Movement", "reference"),
    ]:
        existing_by(doctype, field)
    products, customers, registers, accounts = demo_master_data()
    for product in products:
        existing = existing_by("Inventory Balance", "product")
        if product["name"] not in existing:
            create(
                "Inventory Balance",
                {
                    "product": product["name"],
                    "qty_on_hand": product.get("stock_qty", 100),
                    "qty_reserved": 0,
                    "reorder_level": 20,
                    "reorder_qty": 80,
                    "status": "Active",
                },
            )
    days = []
    cursor = START_DATE
    while cursor <= END_DATE:
        days.append(cursor)
        cursor += timedelta(days=1)
    expense_account = accounts["DEMO-EXP-001"]["name"]
    workers = int(os.environ.get("KORA_DEMO_WORKERS", "24"))
    with concurrent.futures.ThreadPoolExecutor(max_workers=workers) as executor:
        futures = [executor.submit(seed_day, day, products, customers, registers, expense_account) for day in days]
        for future in concurrent.futures.as_completed(futures):
            future.result()
    print(json.dumps({"range": [START_DATE.isoformat(), END_DATE.isoformat()], "days": len(days), "created": dict(sorted(COUNTS.items()))}))


if __name__ == "__main__":
    main()
