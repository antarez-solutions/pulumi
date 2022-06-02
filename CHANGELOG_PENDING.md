### Improvements

- [codegen/go] Handle long and complicated traversals in a nicer way.
  [#9726](https://github.com/pulumi/pulumi/pull/9726)

- [cli] Allow pulumi `destroy -s <stack>` if not in a Pulumi project dir
  [#9613](https://github.com/pulumi/pulumi/pull/9613)

- [engine] Reduce memory usage in convert and yaml programs by caching of package schemas.
  [#9684](https://github.com/pulumi/pulumi/issues/9684)

### Bug Fixes

- [sdk/nodejs] Fix a crash due to dependency cycles from component resources.
  [#9683](https://github.com/pulumi/pulumi/pull/9683)

- [cli/about] Make `pulumi about` aware of the YAML and Java runtimes.
  [#9745](https://github.com/pulumi/pulumi/pull/9745)

- [cli/engine] Fix a panic deserializing resource plans without goals.
  [#9749](https://github.com/pulumi/pulumi/pull/9749)
