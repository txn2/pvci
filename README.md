# pvci

WIP: PVC Injector

**POST** body for `/size`:
```json
{
    "s3_ssl": false,
    "s3_endpoint": "obj-service.data:9000",
    "s3_bucket": "datasets",
    "s3_prefix": "testset",
    "s3_key": "{{DEV_OBJ_KEY}}",
    "s3_secret": "{{DEV_OBJ_SECRET}}"
}
```

**POST** body for `/create`:
```json
{
    "s3_ssl": false,
    "s3_endpoint": "obj-service.data:9000",
    "s3_bucket": "datasets",
    "s3_prefix": "testset",
    "s3_key": "{{DEV_OBJ_KEY}}",
    "s3_secret": "{{DEV_OBJ_SECRET}}",
    "namespace": "default",
    "storage_class": "rook-ceph-block",
    "name": "test-dataset-1"
}
```

**POST** body for `/status`:
```json
{
    "namespace": "default",
    "name": "test-dataset-1"
}
```

**POST** body for `/cleanup`:
```json
{
    "namespace": "default",
    "name": "test-dataset-1"
}
```

**POST** body for `/delete`:
```json
{
    "namespace": "default",
    "name": "test-dataset-1"
}
```

### Test Release

```bash
goreleaser --skip-publish --rm-dist --skip-validate
```

### Release

```bash
GITHUB_TOKEN=$GITHUB_TOKEN goreleaser --rm-dist
```
