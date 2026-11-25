// A guest who is checked in is still in the room: check them out before the booking is deleted.
var doc = __kora_event__.doc;

if (doc.status === "Checked in") {
  throw new Error("Check the guest out first.");
}

return { doc: doc };
