// A delivery is part of the farmer's record and of the product's stock, so it is never
// deleted, not even by the workspace administrator. Correct it with a Stock Adjustment.
var doc = __kora_event__.doc;

throw new Error((doc.reference || doc.name) + " cannot be deleted; correct it with a Stock Adjustment.");
