## API

对外提供GET/PUT/DELETE object的功能



## stream layer

提供Append Only的Stream, 只有partion layer使用, 对外提供AppendBlock和ReadBlock的功能

### build

```
make
```

1. 在子目录stream-manager调用goreman start启动stream manager(默认启动一个)
2. 在子目录extent-node调用goreman start启动nodes(默认启动3个)
3. 在子目录stream-client客户端测试工具: 运行```./stream-client wbench```压测性能

### 架构和IO流程

...

### extent结构

```
extent头(512字节)
	magic number (8字节)
	extent ID    (8字节)
block头 (512字节)
	checksum    (4字节)
	blocklength (4字节) // 所以extent理论最大4G, 实际现在2G
block数据
	数据  (4k对齐)

extent是否seal, 存储在文件系统的attr里面
"seal"=>"true"
```

主要API:
1. OpenExtent
2. AppendBlock
3. ReadBlock
4. Seal


### extent node

管理extents,只知道本地的extent, 和extent对应的副本位置

对外API:

1. AppendBlock (自动复制3副本)
2. ReadBlock

内部集群API:
1. HeartBeat (node之间grpc conn保活)
2. ReplicateBlocks (primary node向secondary node复制副本)
3. AllocExtent  (创建extent, 由stream manager调用)
4. CommitLength (由stream manager调用)
5. Seal (由stream manager调用)

主要内存结构:
1. extendID => localFileName (在启动时打开所有extent的fd)


### stream manager

实现采用embed etcd

API:
```
	rpc StreamInfo(StreamInfoRequest) returns (StreamInfoResponse) {}
	rpc ExtentInfo(ExtentInfoRequest) returns (ExtentInfoResponse) {}
	rpc NodesInfo(NodesInfoRequest) returns(NodesInfoResponse) {}
	rpc StreamAllocExtent(StreamAllocExtentRequest) returns  (StreamAllocExtentResponse) {}
	rpc CreateStream(CreateStreamRequest) returns  (CreateStreamResponse) {}
	rpc RegisterNode(RegisterNodeRequest) returns (RegisterNodeResponse) {}
```




ETCD存储结构:
```
streams/{id} => pb.StreamInfo
nodes/{id}   =>pb.NodeInfo
extents/{id} => pb.ExtentInfo
AutumnSmIDKey 存储已经分配的最大ID
AutumnSmLeader/xxx 存储当前leader的memberValue, 用来在leader写入时校验是否真的是leader
```

内存结构:

#### stream manager 选举

选举成功后:
1. 从etcd中读取数据到内存中
2. 把自己标志成leader

#### stream manager TODO

0. pb.Block可能需要增加offset选项, 保证写入都是幂等的, 这样可以在append block操作的时候, 如果有error, 可以先重试, 而不是直接申请新的extent
1. *实现node hearbteat, 和更精确的alloc policy*
2. *实现GC,检查extent的三副本是否完整和是否extent已经不被任何stream引用*
3. sm的实现中有3个函数很像: sendAllocToNodes, receiveCommitlength, sealExtents 不知道能不能统一
4. *实现Journal*
5. *实现EC*
6. stream manager client的代码可以简化
7. unit test全部缺少
8. 测试多ETCD的情况, 现在只测试了一个ETCD的情况
9. ETCD的key应该改成/clusterKey/node/0, /clusterKey/stream/1的情况, 防止多集群冲突
10. sm的内部数据结构能否改成https://github.com/hashicorp/go-memdb. 在不损失性能的情况下, 提高代码可读性
11. *node支持多硬盘*
12. 在sm里增加version, 每次nodes变化, version加1, 并且在rpc的返回里面增加version, 这样client根据version可以自动更新
13. 增加extent模块benchmark的内容(mac SSD上面, sync 4k需要30ms?!!), 现在benchmark的结果只有4k
14. extent也有很大的优化空间, AppendBlock发到每块硬盘的队列上, 然后取队列, 写数据, 再sync,可以减少单块硬盘上的sync次数. 但是: 如果有SSD
journal的话, 这些优化可能都不需要
15. streamclient增加自动Seal的功能

## partion layer

