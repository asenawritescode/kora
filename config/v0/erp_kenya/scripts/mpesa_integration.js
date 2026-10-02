// M-Pesa Daraja API integration for Kora ERP Kenya
// This script handles M-Pesa transaction verification and auto-matching

var doc = __kora_event__.doc;

// After M-Pesa Payment is created, attempt to auto-match
if (__kora_event__.doctype === "M-Pesa Payment" && __kora_event__.action === "after_insert") {
  // Try to match by reference (phone number -> customer, amount -> invoice)
  var reference = doc.reference || "";
  var phone = doc.phone_number || "";
  var amount = parseFloat(doc.amount || 0);

  try {
    // Search for invoices with matching amount and customer with matching phone
    var invoices = kora.getList("Sales Invoice", {
      filters: [
        {field: "balance_due", op: ">", value: "0"},
        {field: "status", op: "=", value: "Sent"}
      ],
      limit: 50
    });

    for (var i = 0; i < invoices.length; i++) {
      var inv = invoices[i];
      if (Math.abs(parseFloat(inv.balance_due || 0) - amount) < 1.0) {
        // Found matching amount; try to match by phone
        var customerDoc = kora.getDoc("Customer", inv.customer);
        if (customerDoc && (customerDoc.phone === phone || customerDoc.mobile === phone)) {
          // Auto-match!
          kora.saveDoc("M-Pesa Payment", {
            name: doc.name,
            matched: true,
            matched_invoice: inv.name,
            matched_customer: inv.customer,
            status: "Matched"
          });
          break; // Match only first
        }
      }
    }
  } catch (e) {
    // Non-critical; payment stays unmatched for manual review
    kora.log("M-Pesa auto-match error: " + e.message, "warn");
  }
}

// Helper: Make M-Pesa API call (STK Push)
function mpesaSTKPush(phone, amount, reference) {
  // Safaricom Daraja API endpoint
  // Requires consumer key/secret from kora.secrets
  var consumerKey = kora.secrets.get("mpesa_consumer_key");
  var consumerSecret = kora.secrets.get("mpesa_consumer_secret");
  var passkey = kora.secrets.get("mpesa_passkey");
  var shortCode = kora.secrets.get("mpesa_shortcode");

  if (!consumerKey || !consumerSecret) {
    throw new Error("M-Pesa API credentials not configured. Set mpesa_consumer_key and mpesa_consumer_secret in Secrets.");
  }

  // Get OAuth token
  var auth = btoa(consumerKey + ":" + consumerSecret);
  var tokenResp = kora.http.fetch("https://api.safaricom.co.ke/oauth/v1/generate?grant_type=client_credentials", {
    method: "GET",
    headers: {"Authorization": "Basic " + auth}
  });
  var tokenData = JSON.parse(tokenResp.body);
  var accessToken = tokenData.access_token;

  // Generate timestamp and password
  var timestamp = new Date().toISOString().replace(/[-:T.Z]/g, "").substring(0, 14);
  var password = btoa(shortCode + passkey + timestamp);

  // Initiate STK Push
  var stkResp = kora.http.fetch("https://api.safaricom.co.ke/mpesa/stkpush/v1/processrequest", {
    method: "POST",
    headers: {
      "Authorization": "Bearer " + accessToken,
      "Content-Type": "application/json"
    },
    body: JSON.stringify({
      BusinessShortCode: shortCode,
      Password: password,
      Timestamp: timestamp,
      TransactionType: "CustomerPayBillOnline",
      Amount: Math.round(amount),
      PartyA: phone,
      PartyB: shortCode,
      PhoneNumber: phone,
      CallBackURL: "https://your-kora-server.com/api/v1/webhook/mpesa-callback",
      AccountReference: reference || "KORA-" + Date.now(),
      TransactionDesc: "Payment for " + (reference || "Kora ERP")
    })
  });

  return {
    checkout_request_id: JSON.parse(stkResp.body).CheckoutRequestID,
    response: stkResp.body
  };
}
