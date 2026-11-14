package web

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"

	"locless-vhd/proto"
	"locless-vhd/server/imaging"
	"locless-vhd/server/store"
)

// createImage adds an image and its folder inside the image directory.
func (h *server) createImage(w http.ResponseWriter, r *http.Request) {
	var in struct{ Name string }
	if !readJSON(w, r, &in) {
		return
	}
	name := strings.TrimSpace(in.Name)
	if !proto.ValidImageName(name) {
		writeError(w, http.StatusBadRequest, "Имя образа: латиница, цифры, - и _, до 64 символов")
		return
	}
	dir, err := h.store.Setting(r.Context(), store.SettingImageDir)
	if err != nil {
		h.fail(w, err)
		return
	}
	if dir == "" {
		writeError(w, http.StatusConflict, "Сначала укажите папку образов в настройках")
		return
	}
	folder := filepath.Join(dir, name)
	if err := h.store.CreateImage(r.Context(), name, folder); err != nil {
		if errors.Is(err, store.ErrImageExists) {
			writeError(w, http.StatusConflict, "Образ с таким именем уже есть")
			return
		}
		h.fail(w, err)
		return
	}
	if err := os.MkdirAll(folder, 0o755); err != nil {
		h.store.DeleteImage(r.Context(), name)
		writeError(w, http.StatusInternalServerError, "Не удалось создать папку образа")
		return
	}
	h.log.Info("image created", "name", name)
	w.WriteHeader(http.StatusNoContent)
}

// scanResult is what the scan did with one folder of the image directory.
type scanResult struct {
	Image    string   `json:"image"`
	Versions []string `json:"versions"` // files added as versions
	Skipped  []string `json:"skipped"`  // files left out, with the reason
	Note     string   `json:"note,omitempty"`
}

// scanImages looks through the folders of the image directory for version files
// the server does not know, for example an image copied from another server. A
// folder unknown to the server becomes a new image; an image without versions
// gets the ones in its folder. Images with versions are left alone.
func (h *server) scanImages(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	dir, err := h.store.Setting(ctx, store.SettingImageDir)
	if err != nil {
		h.fail(w, err)
		return
	}
	if dir == "" {
		writeError(w, http.StatusConflict, "Сначала укажите папку образов в настройках")
		return
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		writeError(w, http.StatusConflict, "Папка образов недоступна: "+err.Error())
		return
	}
	images, err := h.store.Images(ctx)
	if err != nil {
		h.fail(w, err)
		return
	}
	known := map[string]store.Image{}
	for _, img := range images {
		known[strings.ToLower(img.Name)] = img
	}
	results := []scanResult{}
	for _, e := range entries {
		if !e.IsDir() || !proto.ValidImageName(e.Name()) {
			continue
		}
		folder := filepath.Join(dir, e.Name())
		res := scanResult{Image: e.Name(), Versions: []string{}, Skipped: []string{}}
		img, isKnown := known[strings.ToLower(e.Name())]
		if isKnown {
			res.Image = img.Name
			if len(img.Versions) > 0 {
				continue // the server keeps its versions itself
			}
			if !samePath(img.Path, folder) {
				continue // an image of that name lives elsewhere
			}
			if n, err := h.store.ActiveImageTasks(ctx, img.Name); err != nil || n > 0 {
				res.Note = "образ используется задачей, пропущен"
				results = append(results, res)
				continue
			}
		}
		found, err := imaging.ReadFolder(folder)
		if err != nil {
			res.Note = "папка не читается: " + err.Error()
			results = append(results, res)
			continue
		}
		res.Skipped = append(res.Skipped, found.Skipped...)
		if len(found.Versions) == 0 {
			if len(res.Skipped) > 0 {
				res.Note = "цепочка не найдена"
				results = append(results, res)
			}
			continue
		}
		if !isKnown {
			if err := h.store.CreateImage(ctx, res.Image, folder); err != nil {
				res.Note = "образ не создан: " + err.Error()
				results = append(results, res)
				continue
			}
		}
		for _, v := range found.Versions {
			if err := h.store.AddVersion(ctx, res.Image, v); err != nil {
				res.Note = "версия " + v.File + " не добавлена: " + err.Error()
				break
			}
			res.Versions = append(res.Versions, v.File)
		}
		h.log.Info("image found on disk", "image", res.Image, "versions", len(res.Versions), "skipped", len(res.Skipped))
		results = append(results, res)
	}
	writeJSON(w, http.StatusOK, map[string]any{"found": results})
}

func samePath(a, b string) bool {
	return strings.EqualFold(filepath.Clean(a), filepath.Clean(b))
}

// deleteImage removes an empty image. Versions are never deleted from here.
func (h *server) deleteImage(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if !proto.ValidImageName(name) {
		writeError(w, http.StatusBadRequest, "Неверное имя образа")
		return
	}
	if h.imageBusy(w, r, name) {
		return
	}
	folder, err := h.store.ImagePath(r.Context(), name)
	if err != nil {
		h.fail(w, err)
		return
	}
	err = h.store.DeleteImage(r.Context(), name)
	if errors.Is(err, store.ErrImageInUse) {
		writeError(w, http.StatusConflict, "В образе есть версии, его нельзя удалить")
		return
	}
	if err == nil && folder != "" {
		os.Remove(folder) // an empty folder is all there is; it stays if something else lives in it
	}
	h.done(w, err)
}

// deleteVersion removes a version nothing builds on: the newest one, or a switched
// off one at the end of its line. Every other one is a parent later versions need.
func (h *server) deleteVersion(w http.ResponseWriter, r *http.Request) {
	name, number, ok := versionPath(w, r)
	if !ok || h.imageBusy(w, r, name) {
		return
	}
	versions, err := h.store.AllVersions(r.Context(), name)
	if err != nil {
		h.fail(w, err)
		return
	}
	i := slices.IndexFunc(versions, func(v store.Version) bool { return v.Number == number })
	if i < 0 {
		writeError(w, http.StatusNotFound, "Не найдено")
		return
	}
	if slices.ContainsFunc(versions, func(v store.Version) bool { return v.Parent == number }) {
		writeError(w, http.StatusConflict, "Удалить можно только последнюю версию: от остальных зависят следующие")
		return
	}
	folder, err := h.store.ImagePath(r.Context(), name)
	if err != nil {
		h.fail(w, err)
		return
	}
	if err := os.Remove(filepath.Join(folder, versions[i].File)); err != nil && !errors.Is(err, os.ErrNotExist) {
		writeError(w, http.StatusConflict, "Файл занят: возможно, идёт развёртывание этой версии")
		return
	}
	h.log.Info("version deleted", "image", name, "file", versions[i].File)
	h.done(w, h.store.DeleteVersion(r.Context(), name, number))
}

// versionPath reads the image and the version of a request.
func versionPath(w http.ResponseWriter, r *http.Request) (string, int, bool) {
	name := r.PathValue("name")
	number, err := strconv.Atoi(r.PathValue("number"))
	if err != nil || !proto.ValidImageName(name) {
		writeError(w, http.StatusBadRequest, "Неверная версия")
		return "", 0, false
	}
	return name, number, true
}

// restoreVersion makes an older version the current one; later ones are kept.
func (h *server) restoreVersion(w http.ResponseWriter, r *http.Request) {
	name, number, ok := versionPath(w, r)
	if ok && !h.imageBusy(w, r, name) {
		h.versionResult(w, h.images.Restore(r.Context(), name, number))
	}
}

// enableVersion brings a switched off version back.
func (h *server) enableVersion(w http.ResponseWriter, r *http.Request) {
	name, number, ok := versionPath(w, r)
	if ok && !h.imageBusy(w, r, name) {
		h.versionResult(w, h.images.Enable(r.Context(), name, number))
	}
}

// mergeImage starts folding the chain of an image into one file.
func (h *server) mergeImage(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if !proto.ValidImageName(name) {
		writeError(w, http.StatusBadRequest, "Неверное имя образа")
		return
	}
	if !h.imageBusy(w, r, name) {
		h.versionResult(w, h.images.StartMerge(r.Context(), name))
	}
}

// versionResult tells the operator why the versions of an image did not change.
func (h *server) versionResult(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, imaging.ErrForked):
		writeError(w, http.StatusConflict, "Вернуть нельзя: после отката снят новый образ, цепочка пошла другим путём")
	case errors.Is(err, imaging.ErrBrokenChain):
		h.log.Warn("broken chain", "err", err)
		writeError(w, http.StatusConflict, "Цепочка версий нарушена: файлы не подходят друг к другу или пропали")
	case errors.Is(err, imaging.ErrNothingToMerge):
		writeError(w, http.StatusConflict, "Сливать нечего: в цепочке одна версия")
	case errors.Is(err, imaging.ErrInUse):
		writeError(w, http.StatusConflict, "С образом ещё работает компьютер. Если задачу отменили, сервер отпустит образ в течение минуты")
	case errors.Is(err, imaging.ErrBusy):
		writeError(w, http.StatusConflict, "Образ сейчас занят")
	default:
		h.done(w, err)
	}
}

// imageBusy answers the request itself if a task uses the image: a clone builds
// on its newest version, and a deploy reads its files.
func (h *server) imageBusy(w http.ResponseWriter, r *http.Request, name string) bool {
	if merging, _ := h.images.Merging(name); merging {
		writeError(w, http.StatusConflict, "Идёт слияние версий образа, дождитесь окончания")
		return true
	}
	n, err := h.store.ActiveImageTasks(r.Context(), name)
	if err != nil {
		h.fail(w, err)
		return true
	}
	if n > 0 {
		writeError(w, http.StatusConflict, "Образ используется задачей: дождитесь её окончания или отмените")
		return true
	}
	return false
}

// setImageVHDSize sets the size of the virtual disk a deploy into VHDX makes, in
// whole gibibytes.
func (h *server) setImageVHDSize(w http.ResponseWriter, r *http.Request) {
	var in struct{ GiB uint64 }
	if !readJSON(w, r, &in) {
		return
	}
	if in.GiB != 0 && (in.GiB < store.MinVHDSize>>30 || in.GiB > store.MaxVHDSize>>30) { // before the shift can overflow
		writeError(w, http.StatusBadRequest, fmt.Sprintf("Размер VHDX: 0 (по месту на ПК) или от %d до %d ГБ", store.MinVHDSize>>30, store.MaxVHDSize>>30))
		return
	}
	h.done(w, h.store.SetImageVHDSize(r.Context(), r.PathValue("name"), in.GiB<<30))
}

// setImageKeep sets keep.vhdx of an image: its size in whole gibibytes, 0 turns
// it off, and the folders that go into it.
func (h *server) setImageKeep(w http.ResponseWriter, r *http.Request) {
	var in struct {
		GiB     uint64
		Folders []string
	}
	if !readJSON(w, r, &in) {
		return
	}
	if in.GiB > proto.MaxKeepSize>>30 { // before the shift can overflow
		writeError(w, http.StatusBadRequest, fmt.Sprintf("Размер keep: до %d ГБ", proto.MaxKeepSize>>30))
		return
	}
	folders := []string{}
	for _, f := range in.Folders {
		if f = strings.TrimSpace(f); f != "" {
			folders = append(folders, f)
		}
	}
	err := h.store.SetImageKeep(r.Context(), r.PathValue("name"), proto.Keep{Size: in.GiB << 30, Folders: folders})
	if errors.Is(err, store.ErrBadKeep) {
		writeError(w, http.StatusBadRequest, strings.TrimPrefix(err.Error(), store.ErrBadKeep.Error()+": "))
		return
	}
	h.done(w, err)
}

// setHostImage links a PC to an image, or clears the link.
func (h *server) setHostImage(w http.ResponseWriter, r *http.Request) {
	id, ok := hostID(w, r)
	if !ok {
		return
	}
	var in struct{ Image string }
	if !readJSON(w, r, &in) {
		return
	}
	h.done(w, h.store.SetHostImage(r.Context(), id, in.Image))
}

// imageReady answers the request itself if the image cannot serve a task: its
// files are gone, or a deploy has no version to write.
func (h *server) imageReady(w http.ResponseWriter, r *http.Request, name string, needVersions bool) bool {
	images, err := h.store.Images(r.Context())
	if err != nil {
		h.fail(w, err)
		return false
	}
	if _, err := h.auditImages(r.Context(), images, nil); err != nil {
		h.fail(w, err)
		return false
	}
	i := slices.IndexFunc(images, func(i store.Image) bool { return i.Name == name })
	switch {
	case i >= 0 && images[i].Merging:
		writeError(w, http.StatusConflict, "Идёт слияние версий образа, дождитесь окончания")
	case i >= 0 && images[i].Offline:
		writeError(w, http.StatusConflict, "Папка образа недоступна: "+images[i].Path)
	case i < 0 || images[i].Missing:
		writeError(w, http.StatusConflict, "Файлы образа не найдены")
	case needVersions && !slices.ContainsFunc(images[i].Versions, func(v store.Version) bool { return !v.Disabled }):
		writeError(w, http.StatusConflict, "В образе ещё нет версий, сначала клонируйте эталон")
	default:
		return true
	}
	return false
}

// auditImages compares the images with the disk: it marks what is gone and takes
// such images away from the PCs that used them. Every image is checked at its own
// path. If the place that holds its folder is unreachable, the image is only marked
// offline: a disk that is merely not connected must not cost the PCs their image.
// It returns true if anything is offline, the image directory included.
func (h *server) auditImages(ctx context.Context, images []store.Image, hosts []store.Host) (bool, error) {
	dir, err := h.store.Setting(ctx, store.SettingImageDir)
	if err != nil {
		return false, err
	}
	offline := dir != "" && !isDir(dir)
	for i := range images {
		image := &images[i]
		image.Merging, image.MergeErr = h.images.Merging(image.Name)
		if image.Path == "" {
			continue
		}
		if !isDir(filepath.Dir(image.Path)) {
			image.Offline, offline = true, true
			continue
		}
		image.Missing = !isDir(image.Path)
		for j := range image.Versions {
			v := &image.Versions[j]
			if _, err := os.Stat(filepath.Join(image.Path, v.File)); err != nil {
				v.Missing = true
				image.Missing = image.Missing || !v.Disabled // a switched off version harms nothing
			}
		}
		if !image.Missing {
			continue
		}
		if err := h.store.ClearImageLinks(ctx, image.Name); err != nil {
			return false, err
		}
		for k := range hosts {
			if hosts[k].Image == image.Name {
				hosts[k].Image = ""
			}
		}
	}
	return offline, nil
}

func isDir(path string) bool {
	info, err := os.Stat(path)
	return err == nil && info.IsDir()
}
