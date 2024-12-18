# xixi-bitcask-kv
基于 Bitcask 模型的轻量级 KV 存储引擎，具备读写低时延、高吞吐、超越内存容量的数据存储能力等核心特性，使用 Golang 语言编写。
# 项目亮点
- 支持 B 树、可持久化 B+ 树、自适应基数树索引实现，用户可权衡操作效率与存储能力灵活选择合适的索引方案。
- 支持批量事务写入，通过全局锁和数据库启动识别机制实现事务的原子性与隔离性。
- 设计定制化的日志记录数据格式，采用变长字段并自实现相应的解编码器，优化日志记录的存储效率。
- 运用迭代器模式，在索引层和数据库层分别定义统一的迭代器接口，实现对日志记录的有序遍历和附加操作。
- 引入内存文件映射（MMap）IO管理实现加速索引构建，在数据量不超过可用内存大小的场景下提升启动速度。
# 问题
`Windows`系统中运行时需要确保所有打开的 `DB` 实例或文件显式关闭才能删除文件，否则会出现类似`The process cannot access the file because it is being used by another process.`的错误\
建议在 Mac 或 Linux 环境下运行或在 Windows 系统下测试时手动删除生成的文件
