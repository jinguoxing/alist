package aliyundrive

import (
	"bytes"
	"context"
	"crypto/sha1"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"io"
	"math"
	"math/big"
	"net/http"
	"os"
	"time"

	"github.com/alist-org/alist/v3/drivers/base"
	"github.com/alist-org/alist/v3/internal/conf"
	"github.com/alist-org/alist/v3/internal/driver"
	"github.com/alist-org/alist/v3/internal/errs"
	"github.com/alist-org/alist/v3/internal/model"
	"github.com/alist-org/alist/v3/pkg/cron"
	"github.com/alist-org/alist/v3/pkg/utils"
	"github.com/go-resty/resty/v2"
	log "github.com/sirupsen/logrus"
)

type AliDrive struct {
	model.Storage
	Addition
	AccessToken string
	cron        *cron.Cron
	DriveId     string
}

func (d *AliDrive) Config() driver.Config {
	return config
}

func (d *AliDrive) GetAddition() driver.Additional {
	return d.Addition
}

func (d *AliDrive) Init(ctx context.Context, storage model.Storage) error {
	d.Storage = storage
	err := utils.Json.UnmarshalFromString(d.Storage.Addition, &d.Addition)
	if err != nil {
		return err
	}
	// TODO login / refresh token
	//op.MustSaveDriverStorage(d)
	err = d.refreshToken()
	if err != nil {
		return err
	}
	// get driver id
	res, err, _ := d.request("https://api.aliyundrive.com/v2/user/get", http.MethodPost, nil, nil)
	if err != nil {
		return err
	}
	d.DriveId = utils.Json.Get(res, "default_drive_id").ToString()
	d.cron = cron.NewCron(time.Hour * 2)
	d.cron.Do(func() {
		err := d.refreshToken()
		if err != nil {
			log.Errorf("%+v", err)
		}
	})
	return err
}

func (d *AliDrive) Drop(ctx context.Context) error {
	if d.cron != nil {
		d.cron.Stop()
	}
	return nil
}

func (d *AliDrive) List(ctx context.Context, dir model.Obj, args model.ListArgs) ([]model.Obj, error) {
	files, err := d.getFiles(dir.GetID())
	if err != nil {
		return nil, err
	}
	return utils.SliceConvert(files, func(src File) (model.Obj, error) {
		return fileToObj(src), nil
	})
}

//func (d *AliDrive) Get(ctx context.Context, path string) (model.Obj, error) {
//	// TODO this is optional
//	return nil, errs.NotImplement
//}

func (d *AliDrive) Link(ctx context.Context, file model.Obj, args model.LinkArgs) (*model.Link, error) {
	data := base.Json{
		"drive_id":   d.DriveId,
		"file_id":    file.GetID(),
		"expire_sec": 14400,
	}
	res, err, _ := d.request("https://api.aliyundrive.com/v2/file/get_download_url", http.MethodPost, func(req *resty.Request) {
		req.SetBody(data)
	}, nil)
	if err != nil {
		return nil, err
	}
	return &model.Link{
		Header: http.Header{
			"Referer": []string{"https://www.aliyundrive.com/"},
		},
		URL: utils.Json.Get(res, "url").ToString(),
	}, nil
}

func (d *AliDrive) MakeDir(ctx context.Context, parentDir model.Obj, dirName string) error {
	_, err, _ := d.request("https://api.aliyundrive.com/adrive/v2/file/createWithFolders", http.MethodPost, func(req *resty.Request) {
		req.SetBody(base.Json{
			"check_name_mode": "refuse",
			"drive_id":        d.DriveId,
			"name":            dirName,
			"parent_file_id":  parentDir.GetID(),
			"type":            "folder",
		})
	}, nil)
	return err
}

func (d *AliDrive) Move(ctx context.Context, srcObj, dstDir model.Obj) error {
	err := d.batch(srcObj.GetID(), dstDir.GetID(), "/file/move")
	return err
}

func (d *AliDrive) Rename(ctx context.Context, srcObj model.Obj, newName string) error {
	_, err, _ := d.request("https://api.aliyundrive.com/v3/file/update", http.MethodPost, func(req *resty.Request) {
		req.SetBody(base.Json{
			"check_name_mode": "refuse",
			"drive_id":        d.DriveId,
			"file_id":         srcObj.GetID(),
			"name":            newName,
		})
	}, nil)
	return err
}

func (d *AliDrive) Copy(ctx context.Context, srcObj, dstDir model.Obj) error {
	err := d.batch(srcObj.GetID(), dstDir.GetID(), "/file/copy")
	return err
}

func (d *AliDrive) Remove(ctx context.Context, obj model.Obj) error {
	_, err, _ := d.request("https://api.aliyundrive.com/v2/recyclebin/trash", http.MethodPost, func(req *resty.Request) {
		req.SetBody(base.Json{
			"drive_id": d.DriveId,
			"file_id":  obj.GetID(),
		})
	}, nil)
	return err
}

func (d *AliDrive) Put(ctx context.Context, dstDir model.Obj, stream model.FileStreamer, up driver.UpdateProgress) error {
	file := model.FileStream{
		Obj:        stream,
		ReadCloser: stream,
		Mimetype:   stream.GetMimetype(),
	}
	const DEFAULT int64 = 10485760
	var count = int(math.Ceil(float64(stream.GetSize()) / float64(DEFAULT)))

	partInfoList := make([]base.Json, 0, count)
	for i := 1; i <= count; i++ {
		partInfoList = append(partInfoList, base.Json{"part_number": i})
	}
	reqBody := base.Json{
		"check_name_mode": "overwrite",
		"drive_id":        d.DriveId,
		"name":            file.GetName(),
		"parent_file_id":  dstDir.GetID(),
		"part_info_list":  partInfoList,
		"size":            file.GetSize(),
		"type":            "file",
	}

	if d.RapidUpload {
		buf := bytes.NewBuffer(make([]byte, 0, 1024))
		io.CopyN(buf, file, 1024)
		reqBody["pre_hash"] = utils.GetSHA1Encode(buf.String())
		// 把头部拼接回去
		file.ReadCloser = struct {
			io.Reader
			io.Closer
		}{
			Reader: io.MultiReader(buf, file),
			Closer: file,
		}
	} else {
		reqBody["content_hash_name"] = "none"
		reqBody["proof_version"] = "v1"
	}

	var resp UploadResp
	_, err, e := d.request("https://api.aliyundrive.com/adrive/v2/file/createWithFolders", http.MethodPost, func(req *resty.Request) {
		req.SetBody(reqBody)
	}, &resp)

	if err != nil && e.Code != "PreHashMatched" {
		return err
	}

	if d.RapidUpload && e.Code == "PreHashMatched" {
		tempFile, err := os.CreateTemp(conf.Conf.TempDir, "file-*")
		if err != nil {
			return err
		}
		defer func() {
			_ = tempFile.Close()
			_ = os.Remove(tempFile.Name())
		}()
		delete(reqBody, "pre_hash")
		h := sha1.New()
		if _, err = io.Copy(io.MultiWriter(tempFile, h), file); err != nil {
			return err
		}
		reqBody["content_hash"] = hex.EncodeToString(h.Sum(nil))
		reqBody["content_hash_name"] = "sha1"
		reqBody["proof_version"] = "v1"

		/*
			js 隐性转换太坑不知道有没有bug
			var n = e.access_token，
			r = new BigNumber('0x'.concat(md5(n).slice(0, 16)))，
			i = new BigNumber(t.file.size)，
			o = i ? r.mod(i) : new gt.BigNumber(0);
			(t.file.slice(o.toNumber(), Math.min(o.plus(8).toNumber(), t.file.size)))
		*/
		buf := make([]byte, 8)
		r, _ := new(big.Int).SetString(utils.GetMD5Encode(d.AccessToken)[:16], 16)
		i := new(big.Int).SetInt64(file.GetSize())
		o := r.Mod(r, i)
		n, _ := io.NewSectionReader(tempFile, o.Int64(), 8).Read(buf[:8])
		reqBody["proof_code"] = base64.StdEncoding.EncodeToString(buf[:n])

		_, err, e := d.request("https://api.aliyundrive.com/adrive/v2/file/createWithFolders", http.MethodPost, func(req *resty.Request) {
			req.SetBody(reqBody)
		}, &resp)
		if err != nil && e.Code != "PreHashMatched" {
			return err
		}
		if resp.RapidUpload {
			return nil
		}
		// 秒传失败
		if _, err = tempFile.Seek(0, io.SeekStart); err != nil {
			return err
		}
		file.ReadCloser = tempFile
	}

	for i, partInfo := range resp.PartInfoList {
		req, err := http.NewRequest("PUT", partInfo.UploadUrl, io.LimitReader(file, DEFAULT))
		if err != nil {
			return err
		}
		res, err := base.HttpClient.Do(req)
		if err != nil {
			return err
		}
		res.Body.Close()
		if count > 0 {
			up(i * 100 / count)
		}
	}
	var resp2 base.Json
	_, err, e = d.request("https://api.aliyundrive.com/v2/file/complete", http.MethodPost, func(req *resty.Request) {
		req.SetBody(base.Json{
			"drive_id":  d.DriveId,
			"file_id":   resp.FileId,
			"upload_id": resp.UploadId,
		})
	}, &resp2)
	if err != nil && e.Code != "PreHashMatched" {
		return err
	}
	if resp2["file_id"] == resp.FileId {
		return nil
	}
	return fmt.Errorf("%+v", resp2)
}

func (d *AliDrive) Other(ctx context.Context, args model.OtherArgs) (interface{}, error) {
	var resp base.Json
	var url string
	data := base.Json{
		"drive_id": d.DriveId,
		"file_id":  args.Obj.GetID(),
	}
	switch args.Method {
	case "doc_preview":
		url = "https://api.aliyundrive.com/v2/file/get_office_preview_url"
		data["access_token"] = d.AccessToken
	case "video_preview":
		url = "https://api.aliyundrive.com/v2/file/get_video_preview_play_info"
		data["category"] = "live_transcoding"
	default:
		return nil, errs.NotSupport
	}
	_, err, _ := d.request(url, http.MethodPost, func(req *resty.Request) {
		req.SetBody(data)
	}, &resp)
	if err != nil {
		return nil, err
	}
	return resp, nil
}

var _ driver.Driver = (*AliDrive)(nil)
