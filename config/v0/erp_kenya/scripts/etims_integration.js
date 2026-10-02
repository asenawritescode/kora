// eTIMS (electronic Tax Invoice Management System) integration
// KRA eTIMS API integration for invoice submission

var doc = __kora_event__.doc;

// When a Sales Invoice is submitted, queue it with DigiTax.
if (__kora_event__.doctype === "Sales Invoice" && (__kora_event__.action === "after_submit" || __kora_event__.action === "on_submit")) {
  submitToETIMS(doc);
}

function submitToETIMS(invoiceDoc) {
  try {
    var customer = kora.getDoc("Customer", invoiceDoc.customer);
    var items = kora.getList("Invoice Item", {
      filters: [{field: "parent", op: "=", value: invoiceDoc.name}]
    });

    var traderInvoiceNumber = invoiceDoc.name.replace(/[^A-Za-z0-9_.-]/g, "-");
    var callbackURL = kora.secrets.get("digitax_callback_url");
    var etimsPayload = {
      sale_date: invoiceDoc.invoice_date,
      customer_tin: customer ? customer.tax_id || "" : "",
      customer_name: customer ? customer.customer_name : "",
      trader_invoice_number: traderInvoiceNumber,
      payment_type_code: "07",
      invoice_status_code: "01",
      callback_url: callbackURL || undefined,
      invoice_details: invoiceDoc.notes || invoiceDoc.terms || "",
      is_tax_exempt: false,
      items: []
    };

    for (var i = 0; i < items.length; i++) {
      var itemDoc = kora.getDoc("Item", items[i].item);
      etimsPayload.items.push({
        item_id: itemDoc ? itemDoc.digitax_item_id : "",
        quantity: items[i].quantity,
        unit_price: items[i].unit_price,
        discount_rate: 0
      });
    }

    // Create eTIMS Invoice record
    var etimsDoc = {
      sales_invoice: invoiceDoc.name,
      provider: "DigiTax",
      trader_invoice_number: traderInvoiceNumber,
      submission_date: new Date().toISOString(),
      status: "Pending",
      provider_status: "pending",
      retry_count: 0
    };

    // Submit to eTIMS API
    var etimsBaseUrl = kora.secrets.get("digitax_api_url") || "https://api.digitax.tech/ke/v2";
    var apiKey = kora.secrets.get("digitax_api_key");

    if (!apiKey) {
      kora.log("DigiTax API key not configured. Set digitax_api_key in Secrets.", "warn");
      etimsDoc.status = "Draft";
      etimsDoc.error_message = "DigiTax API key not configured";
      kora.saveDoc("eTIMS Invoice", etimsDoc);
      return;
    }

    var response = kora.http.fetch(etimsBaseUrl + "/sales", {
      method: "POST",
      headers: {
        "X-API-Key": apiKey,
        "Content-Type": "application/json"
      },
      body: JSON.stringify(etimsPayload)
    });

    var respData = JSON.parse(response.body);
    if (response.status === 200 || response.status === 201) {
      etimsDoc.provider_sale_id = respData.id || respData.sale_id || "";
      etimsDoc.provider_status = (respData.queue_status || respData.status || "pending").toLowerCase();
      etimsDoc.status = "Pending";
      etimsDoc.submission_response = JSON.stringify(respData);
    } else {
      etimsDoc.status = "Failed";
      etimsDoc.provider_status = "failed";
      etimsDoc.error_message = respData.message || "DigiTax submission failed with status " + response.status;
      etimsDoc.retry_count = 1;
    }

    kora.saveDoc("eTIMS Invoice", etimsDoc);

  } catch (e) {
    kora.log("eTIMS submission error for " + invoiceDoc.name + ": " + e.message, "error");
    // Create failed record
    kora.saveDoc("eTIMS Invoice", {
      sales_invoice: invoiceDoc.name,
      status: "Failed",
      error_message: e.message,
      retry_count: 1
    });
  }
}

// Retry failed eTIMS submissions
function retryFailedETIMS() {
  var failed = kora.getList("eTIMS Invoice", {
    filters: [
      {field: "status", op: "=", value: "Failed"},
      {field: "retry_count", op: "<", value: "5"}
    ],
    limit: 10
  });

  for (var i = 0; i < failed.length; i++) {
    var invoice = kora.getDoc("Sales Invoice", failed[i].sales_invoice);
    if (invoice) {
      submitToETIMS(invoice);
      // Update retry count
      kora.saveDoc("eTIMS Invoice", {
        name: failed[i].name,
        retry_count: (parseInt(failed[i].retry_count || 0)) + 1,
        last_retry: new Date().toISOString()
      });
    }
  }
}
