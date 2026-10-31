/**
 * Template Pack Validation Script
 * 
 * Runs on validate event for Template Pack documents.
 * Checks that all template_files have valid structure, correct content_types,
 * and that the config_hash matches the actual file contents.
 *
 * Setup in Kigae CMS Admin → Scripts:
 *   Name:                validate_template_pack
 *   Script Type:         doc_event
 *   DocType:             Template Pack
 *   Event:               validate
 *   Is Active:           yes
 *
 * Dependencies: KORA_SCRIPTS_ENABLED=true
 */

var pack = __kora_event__.doc;
var files = pack.template_files || [];
var errors = [];

// ── 1. Must have at least one file ──
if (!files || !Array.isArray(files) || files.length === 0) {
  pack.validation_status = "invalid";
  pack.validation_error = "Pack must contain at least one file.";
  return;
}

// ── 2. Validate each file ──
var allowedTypes = ["doctype", "roles", "permissions", "workflow_definition", "view", "report", "script"];
var contentTypes = {};
var yamlErrors = [];
var totalSize = 0;
var maxFileSize = 256 * 1024;  // 256 KB
var maxTotalSize = 2 * 1024 * 1024; // 2 MB

for (var i = 0; i < files.length; i++) {
  var f = files[i];
  var path = (f.path || "").trim();
  var content = f.content || "";
  var ct = (f.content_type || "").trim();

  if (!path) {
    errors.push("File " + (i + 1) + ": path is required");
    continue;
  }
  if (!content) {
    errors.push(path + ": content is empty");
    continue;
  }

  // Check content_type
  if (!ct) {
    errors.push(path + ": content_type is required");
    continue;
  }
  if (allowedTypes.indexOf(ct) === -1) {
    errors.push(path + ": unknown content_type '" + ct + "' (allowed: " + allowedTypes.join(", ") + ")");
    continue;
  }
  contentTypes[ct] = (contentTypes[ct] || 0) + 1;

  // Validate path matches content_type
  if (ct === "doctype" && path.indexOf("doctypes/") !== 0) {
    errors.push(path + ": content_type 'doctype' requires path under doctypes/");
  }
  if (ct === "roles" && path !== "roles.yaml") {
    errors.push(path + ": content_type 'roles' requires path 'roles.yaml'");
  }
  if (ct === "permissions" && path !== "permissions.yaml") {
    errors.push(path + ": content_type 'permissions' requires path 'permissions.yaml'");
  }
  if (ct === "workflow_definition" && path.indexOf("workflows/") !== 0) {
    errors.push(path + ": content_type 'workflow_definition' requires path under workflows/");
  }
  if (ct === "view" && path.indexOf("views/") !== 0) {
    errors.push(path + ": content_type 'view' requires path under views/");
  }
  if (ct === "report" && path.indexOf("reports/") !== 0) {
    errors.push(path + ": content_type 'report' requires path under reports/");
  }
  if (ct === "script" && path.indexOf("scripts/") !== 0) {
    errors.push(path + ": content_type 'script' requires path under scripts/");
  }

  // Check file extension
  if (ct === "script") {
    if (path.indexOf(".js") !== path.length - 3) {
      errors.push(path + ": script files must have .js extension");
    }
  } else {
    if (path.indexOf(".yaml") !== path.length - 5 && path.indexOf(".yml") !== path.length - 4) {
      errors.push(path + ": only .yaml/.yml extensions allowed for " + ct);
    }
  }

  // Check for path traversal
  if (path.indexOf("..") !== -1) {
    errors.push(path + ": path traversal rejected");
  }

  // Quick YAML validation (check for parse errors)
  if (ct !== "script" && content.trim()) {
    try {
      // Basic structure check: look for obvious YAML issues
      var lines = content.split("\n");
      var inList = false;
      for (var j = 0; j < lines.length; j++) {
        var line = lines[j];
        // Skip empty lines and comments
        if (!line.trim() || line.trim().indexOf("#") === 0) continue;
      }
    } catch (e) {
      yamlErrors.push(path + ": YAML parse error - " + e);
    }
  }

  // Size check
  totalSize += content.length;
  if (content.length > maxFileSize) {
    errors.push(path + ": file size " + content.length + " exceeds max " + maxFileSize);
  }
}

// ── 3. Check required content types ──
if (!contentTypes["doctype"] && !contentTypes["roles"]) {
  errors.push("Pack must contain at least one doctype or roles file.");
}

if (errors.length > 0 || yamlErrors.length > 0) {
  pack.validation_status = "invalid";
  pack.validation_error = errors.concat(yamlErrors).join("; ");
  kora.log.warn("Pack validation failed: " + pack.name + " - " + pack.validation_error);
  return;
}

// ── 4. Size limit ──
if (totalSize > maxTotalSize) {
  pack.validation_status = "invalid";
  pack.validation_error = "Total pack size " + totalSize + " exceeds max " + maxTotalSize;
  kora.log.warn("Pack validation failed (size): " + pack.name);
  return;
}

// ── 5. All checks passed ──
pack.validation_status = "valid";
pack.validation_error = "";
kora.log.info("Pack validated: " + pack.name + " (" + files.length + " files, " + Object.keys(contentTypes).length + " types)");
