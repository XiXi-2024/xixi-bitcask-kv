package data

import (
	"errors"
	"fmt"
	"github.com/XiXi-2024/xixi-kv/fio"
	"hash/crc32"
	"io"
	"path/filepath"
)

var (
	ErrInvalidCRC = errors.New("invalid crc value, log record maybe corrupted")
)

const (
	// DataFileNameSuffix 数据文件后缀
	DataFileNameSuffix = ".data"

	// HintFileName Hint文件全名
	HintFileName = "hint-index"

	// MergeFinishedFileName merge完成标识文件全名
	MergeFinishedFileName = "merge-finished"

	// SeqNoFileName 事务序列号文件全名
	SeqNoFileName = "merge-finished"
)

// DataFile 数据文件
type DataFile struct {
	FileId     uint32         // 文件 id
	WriteOff   int64          // 文件数据末尾偏移量, 供活跃文件执行写入操作
	ReadWriter fio.ReadWriter // IO 实现
}

// OpenDataFile 打开数据文件
// todo 优化点：重构除去不是必须的
func OpenDataFile(dirPath string, fileId uint32, ioType fio.FileIOType) (*DataFile, error) {
	fileName := GetDataFileName(dirPath, fileId)
	return newDataFile(fileName, fileId, ioType)
}

// OpenHintFile 打开 Hint 索引文件
func OpenHintFile(dirPath string) (*DataFile, error) {
	fileName := filepath.Join(dirPath, HintFileName)
	return newDataFile(fileName, 0, fio.StandardFIO)
}

// OpenMergeFinishedFile 打开 merge 完成标识文件
func OpenMergeFinishedFile(dirPath string) (*DataFile, error) {
	fileName := filepath.Join(dirPath, MergeFinishedFileName)
	return newDataFile(fileName, 0, fio.StandardFIO)
}

// OpenSeqNoFile 打开事务序列号文件并构造 DataFile 实例
func OpenSeqNoFile(dirPath string) (*DataFile, error) {
	fileName := filepath.Join(dirPath, SeqNoFileName)
	return newDataFile(fileName, 0, fio.StandardFIO)
}

// GetDataFileName 获取完整数据文件名称
func GetDataFileName(dirPath string, fileId uint32) string {
	return filepath.Join(dirPath, fmt.Sprintf("%09d", fileId)+DataFileNameSuffix)
}

// 根据完整文件名称打开文件并构造 DataFile 实例
func newDataFile(fileName string, fileId uint32, ioType fio.FileIOType) (*DataFile, error) {
	// 根据配置的类型和路径创建新 IO 管理器实例
	readWriter, err := fio.NewReadWriter(fileName, ioType)
	if err != nil {
		return nil, err
	}

	// 构造该文件的 DataFile 实例并返回
	return &DataFile{
		FileId:     fileId,
		WriteOff:   0,
		ReadWriter: readWriter,
	}, nil
}

// ReadLogRecord 从偏移量 offset 开始读取一条日志记录
func (df *DataFile) ReadLogRecord(offset int64) (*LogRecord, int64, error) {
	// 获取当前文件总长度
	fileSize, err := df.ReadWriter.Size()
	if err != nil {
		return nil, 0, err
	}
	// 以固定最大长度读取 Header 头部, 只需保证完整包括一条日志记录即可
	var headerBytes int64 = maxLogRecordHeaderSize
	// 如果固定最大长度超过文件剩余长度则读取剩余长度数据, 避免 EOF
	if offset+maxLogRecordHeaderSize > fileSize {
		headerBytes = fileSize - offset
	}
	headerBuf, err := df.readNBytes(headerBytes, offset)
	if err != nil {
		return nil, 0, err
	}

	// 解码
	header, headerSize := decodeLogRecordHeader(headerBuf)
	// 解码失败, 读取的字节不包含新的reader头部, 表示已读取到文件末尾
	// todo 优化点：根据写入偏移量判断, 避免额外的磁盘IO
	if header == nil {
		return nil, 0, io.EOF
	}
	if header.crc == 0 && header.keySize == 0 && header.valueSize == 0 {
		return nil, 0, io.EOF
	}

	// 获取 key 和 value 长度
	keySize, valueSize := int64(header.keySize), int64(header.valueSize)
	// 计算日志记录总长度
	var recordSize = headerSize + keySize + valueSize

	// 读取数据部分, 构建 logRecord 实例
	logRecord := &LogRecord{Type: header.recordType}
	kvBuf, err := df.readNBytes(keySize+valueSize, offset+headerSize)
	if err != nil {
		return nil, 0, err
	}
	logRecord.Key = kvBuf[:keySize]
	logRecord.Value = kvBuf[keySize:]

	// 校验数据完整性, 生成 CRC 值进行比较
	crc := getLogRecordCRC(logRecord, headerBuf[crc32.Size:headerSize])
	if crc != header.crc {
		return nil, 0, ErrInvalidCRC
	}

	return logRecord, recordSize, nil
}

// Write 文件写入
func (df *DataFile) Write(buf []byte) error {
	n, err := df.ReadWriter.Write(buf)
	if err != nil {
		return err
	}
	// 更新写入偏移量
	df.WriteOff += int64(n)
	return nil
}

// WriteHintRecord 写入构建索引所需的相关数据
func (df *DataFile) WriteHintRecord(key []byte, pos *LogRecordPos) error {
	// 转换为对应的 LogRecord 实例进行写入
	record := &LogRecord{
		Key:   key,
		Value: EncodeLogRecordPos(pos),
	}
	encRecord, _ := EncodeLogRecord(record)
	return df.Write(encRecord)
}

// Sync 文件持久化
func (df *DataFile) Sync() error {
	return df.ReadWriter.Sync()
}

// Close 文件关闭
func (df *DataFile) Close() error {
	return df.ReadWriter.Close()
}

// 从偏移量 offset 开始读取 n 个字节
func (df *DataFile) readNBytes(n int64, offset int64) (b []byte, err error) {
	b = make([]byte, n)
	_, err = df.ReadWriter.Read(b, offset)
	return b, err
}
