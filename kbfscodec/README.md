## kbfscodec

The serialization logic for KBFS. The package is named `kbfscodec`
rather than `codec` for two reasons:

1. We already have a `keybase/go-codec/codec` that is commonly used.
2. A lot of variables are named `codec` already.
