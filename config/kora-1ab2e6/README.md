# Demo tenant configuration: kora-1ab2e6

This is an exported Kora config pack for the existing local demo tenant `kora-1ab2e6` (POS page at `/s/kora-1ab2e6/workspace/pos`). It includes the built-in `category_tabs` component, bound to `Product.category`, before the existing product grid.

Export the active tenant config before changes:

```sh
kora config export --site kora-1ab2e6 --path /tmp/kora-1ab2e6-config-before
```

Apply this pack:

```sh
kora config import --site kora-1ab2e6 --path config/kora-1ab2e6
```

Config import activates a new config version. Roll back by importing the saved pre-change export, or use `kora config rollback --help` after checking `kora config versions --site kora-1ab2e6`.
