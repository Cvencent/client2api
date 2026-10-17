package core

// VirtualModelClient is the reserved client namespace under which the panel and
// the gateway store metadata for gateway-level virtual models.  Auto/<model>
// and operator-declared model groups have no module of their own, so their
// manual context/output overrides need a key that cannot collide with a real
// platform name.
const VirtualModelClient = "__virtual__"
