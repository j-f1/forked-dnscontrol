---
name: DEFAULTS
parameters:
  - modifiers...
parameter_types:
  "modifiers...": RecordModifier[]
---

`DEFAULTS` allows you to declare a set of default arguments to apply to all subsequent domains. Subsequent calls to [D](#D) will have these
arguments passed as if they were the first modifiers in the argument list.

{% capture example %}
```js
var COMMON = NewDnsProvider("foo");
// we want to create backup zone files for all domains, but not actually register them.
// also create a default TTL
DEFAULTS( DnsProvider(COMMON,0), DefaultTTL(1000));

D("example.com", REGISTRAR, DnsProvider("R53"), A("@","1.2.3.4")); // this domain will have the defaults set.

// clear defaults
DEFAULTS();
D("example2.com", REGISTRAR, DnsProvider("R53"), A("@","1.2.3.4")); // this domain will not have the previous defaults.
```
{% endcapture %}

{% include example.html content=example %}
