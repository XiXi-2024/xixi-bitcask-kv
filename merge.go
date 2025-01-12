package xixi_bitcask_kv

import (
	"github.com/XiXi-2024/xixi-bitcask-kv/data"
	"github.com/XiXi-2024/xixi-bitcask-kv/utils"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
)

const (
	// merge 临时目录名称后缀
	mergeDirName = "-merge"

	// merge 完成标识文件中的未参与 merge 的最近文件 key
	mergeFinishedKey = "merge.finished"
)

// todo 当前仅支持显式触发, 后续可实现定时任务, 监控数据状态, 进行自动清理
func (db *DB) Merge() error {
	// 不存在活跃文件时, 数据库为空, 直接返回
	if db.activeFile == nil {
		return nil
	}

	db.mu.Lock()
	defer db.mu.Unlock()

	// merge 状态为正在进行中, 返回错误
	if db.isMerging {
		db.mu.Unlock()
		return ErrMergeIsProgress
	}

	// 判断无效数据占比是否达到阈值, 未达到则不执行 merge
	totalSize, err := utils.DirSize(db.options.DirPath)
	if err != nil {
		db.mu.Unlock()
		return err
	}
	if float32(db.reclaimSize)/float32(totalSize) < db.options.DataFileMergeRatio {
		db.mu.Unlock()
		return ErrMergeRatioUnreached
	}

	// 判断数据目录所在磁盘剩余空间是否能容纳 merge 后的数据量
	availableDiskSize, err := utils.AvailableDiskSizeWin(db.options.DirPath)
	if err != nil {
		db.mu.Unlock()
		return err
	}
	if uint64(totalSize-db.reclaimSize) >= availableDiskSize {
		db.mu.Unlock()
		return ErrNoEnoughSpaceForMerge
	}

	// 更新 merge 状态
	db.isMerging = true
	defer func() {
		db.isMerging = false
	}()

	// 创建新活跃文件, 令原活跃文件同样参与 merge
	if err := db.activeFile.Sync(); err != nil {
		db.mu.Unlock()
		return err
	}
	db.olderFiles[db.activeFile.FileId] = db.activeFile
	if err := db.setActiveDataFile(); err != nil {
		db.mu.Unlock()
		return nil
	}

	// 记录未参与 merge 的最近文件 id
	nonMergeFileId := db.activeFile.FileId

	// 收集所有参与 merge 的数据文件实例
	var mergeFiles []*data.DataFile
	for _, file := range db.olderFiles {
		mergeFiles = append(mergeFiles, file)
	}
	db.mu.Unlock()

	// 升序排序, 供后续顺序 merge
	// todo 无效操作, 可取消
	sort.Slice(mergeFiles, func(i, j int) bool {
		return mergeFiles[i].FileId < mergeFiles[j].FileId
	})

	// 获取 merge 临时目录路径
	mergePath := db.getMergePath()
	// 如果存在上次 merge 的残留目录, 将其删除
	if _, err := os.Stat(mergePath); err == nil {
		if err := os.RemoveAll(mergePath); err != nil {
			return err
		}
	}

	// 新建 merge 临时目录
	if err := os.MkdirAll(mergePath, os.ModePerm); err != nil {
		return err
	}

	// 创建新的临时 DB 实例操作临时目录, 避免并发冲突
	mergeOptions := db.options
	mergeOptions.DirPath = mergePath
	mergeOptions.SyncWrites = false // 加快 merge 速度
	mergeDB, err := Open(mergeOptions)
	if err != nil {
		return err
	}

	// 获取 hint 索引文件实例
	hintFile, err := data.OpenHintFile(mergePath)
	if err != nil {
		return err
	}

	// 遍历每个数据文件中的每个日志记录
	for _, dataFile := range mergeFiles {
		var offset int64 = 0
		for {
			logRecord, size, err := dataFile.ReadLogRecord(offset)
			if err != nil {
				if err == io.EOF {
					break
				}
				return err
			}
			// 解析得到真实 key
			realKey, _ := parseLogRecordKey(logRecord.Key)
			logRecordPos := db.index.Get(realKey)
			// 与索引中的最新数据比较, 如果是有效数据则重写
			if logRecordPos != nil &&
				logRecordPos.Fid == dataFile.FileId && logRecordPos.Offset == offset {
				// 对于有效数据, 直接清除事务标记
				// todo 不应该在这里清除
				logRecord.Key = logRecordKeyWithSeq(realKey, nonTransactionSeqNo)
				// 将当前有效数据重写到 merge 临时目录的活跃文件中
				pos, err := mergeDB.appendLogRecord(logRecord)
				if err != nil {
					return err
				}
				// 顺便的将构建索引所需信息写入 Hint 文件中 用于后续重启时加速构建索引
				if err := hintFile.WriteHintRecord(realKey, pos); err != nil {
					return err
				}
			}
			offset += size
		}
	}

	// 将重写的数据文件和 hint 文件持久化
	if err := hintFile.Sync(); err != nil {
		return err
	}
	if err := mergeDB.Sync(); err != nil {
		return err
	}

	// 新增 merge 完成标识文件
	mergeFinishedFile, err := data.OpenMergeFinishedFile(mergePath)
	if err != nil {
		return err
	}
	// 向文件写入未参与该次 merge 的最近数据文件id
	mergeFinRecord := &data.LogRecord{
		Key:   []byte(mergeFinishedKey), // key 为固定常量
		Value: []byte(strconv.Itoa(int(nonMergeFileId))),
	}

	// 编码后写入
	encRecord, _ := data.EncodeLogRecord(mergeFinRecord)
	if err := mergeFinishedFile.Write(encRecord); err != nil {
		return err
	}
	if err := mergeFinishedFile.Sync(); err != nil {
		return err
	}

	return nil
}

// 获取 merge 临时目录路径, 与数据目录同级
func (db *DB) getMergePath() string {
	// 获取数据目录的父目录路径
	dir := filepath.Dir(filepath.Clean(db.options.DirPath))
	// 获取数据目录名称
	base := filepath.Base(db.options.DirPath)
	return filepath.Join(dir, base+mergeDirName)
}

// 加载 merge 临时目录中的数据文件
func (db *DB) loadMergeFiles() error {
	mergePath := db.getMergePath()
	// 目录不存在, 不存在未完成的 merge, 直接返回
	if _, err := os.Stat(mergePath); os.IsNotExist(err) {
		return nil
	}

	defer func() {
		// 加载完成后删除临时目录
		_ = os.RemoveAll(mergePath)
	}()

	// 读取目录中所有文件
	dirEntries, err := os.ReadDir(mergePath)
	if err != nil {
		return err
	}

	// 是否存在 merge 完成文件标识
	var mergeFinished bool
	// 参与 merge 的数据文件名称集合
	var mergeFileNames []string
	for _, entry := range dirEntries {
		if entry.Name() == data.MergeFinishedFileName {
			mergeFinished = true
		}
		// 过滤事务 id 文件, 其中包含的事务 id 非最新, 加载无意义
		if entry.Name() == data.SeqNoFileName {
			continue
		}
		// 过滤文件锁文件
		if entry.Name() == fileLockName {
			continue
		}
		mergeFileNames = append(mergeFileNames, entry.Name())
	}

	// merge 未完成直接返回
	if !mergeFinished {
		return nil
	}

	// 从标识文件中取出未参与 merge 的最近数据文件 id
	nonMergeFileId, err := db.getNonMergeFileId(mergePath)
	if err != nil {
		return err
	}

	// 删除参与 merge 的旧数据文件
	var fileId uint32 = 0
	for ; fileId < nonMergeFileId; fileId++ {
		// 获取完整数据文件名称
		fileName := data.GetDataFileName(db.options.DirPath, fileId)
		// 如果存在则删除
		if _, err := os.Stat(fileName); err == nil {
			if err := os.Remove(fileName); err != nil {
				return err
			}
		}
	}

	// 将新的数据文件移动到数据目录中
	for _, fileName := range mergeFileNames {
		srcPath := filepath.Join(mergePath, fileName)
		destPath := filepath.Join(db.options.DirPath, fileName)
		if err := os.Rename(srcPath, destPath); err != nil {
			return err
		}
	}

	return nil
}

// 获取 merge 完成标识文件中保存的未参与 merge 的最近数据文件id
// todo 是否可重构为取消 dirPath 参数
func (db *DB) getNonMergeFileId(dirPath string) (uint32, error) {
	mergeFinishedFile, err := data.OpenMergeFinishedFile(dirPath)
	if err != nil {
		return 0, err
	}
	record, _, err := mergeFinishedFile.ReadLogRecord(0)
	if err != nil {
		return 0, err
	}
	nonMergeFileId, err := strconv.Atoi(string(record.Value))
	if err != nil {
		return 0, err
	}
	return uint32(nonMergeFileId), nil
}

// 通过 hint 文件加载索引
func (db *DB) loadIndexFromHintFile() error {
	// 判断 hint 文件是否存在
	hintFileName := filepath.Join(db.options.DirPath, data.HintFileName)
	if _, err := os.Stat(hintFileName); os.IsNotExist(err) {
		return nil
	}

	// 打开 hint 文件
	hintFile, err := data.OpenHintFile(hintFileName)
	if err != nil {
		return err
	}

	// 读取文件 遍历日志记录
	var offset int64 = 0
	for {
		logRecord, size, err := hintFile.ReadLogRecord(offset)
		if err != nil {
			if err == io.EOF {
				break
			}
			return err
		}

		// 解码得到索引位置信息, 加载索引
		pos := data.DecodeLogRecordPos(logRecord.Value)
		db.index.Put(logRecord.Key, pos)
		offset += size
	}

	return nil
}
