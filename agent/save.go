package main

import (
	"errors"
	"fmt"

	"locless-vhd/agent/ui"
	"locless-vhd/proto"
	"locless-vhd/recovery"
)

// Save Mode makes this PC the master of its image. Windows boots from save.vhdx
// on top of the base, so what the operator installs survives reboots, and it
// holds nothing of local: the stretched volume, the name and the settings of
// this PC never reach the image. See proto.TaskKindSaveMode.
//
// Saving is an ordinary clone of the disk Windows sees in Save Mode, the base
// with save.vhdx on top, attached in WinPE. It shrinks the volume and sends what
// the server lacks, as any clone. Then the server queues a deploy of the new
// version for this PC: the base becomes the version on the server block for
// block, through the same deploy every PC gets, and the usual chain comes back.

// savedStore is the store of this PC for the tasks of Save Mode.
type savedStore interface {
	State() proto.StoreState
	// EnterSave makes save.vhdx on top of the base, stretches the Windows volume
	// in it, installs the reset service there and boots it.
	EnterSave() error
	// LeaveSave boots a fresh differencing disk on top of local again and removes
	// save.vhdx. The base was not changed, so local is still good.
	LeaveSave(accounts *proto.Accounts) error
	// OpenSaved attaches the base with save.vhdx on top as a disk to clone.
	OpenSaved() (source, error)
	Close() error
}

var saveStages = []string{"Сеть", "Сервер", "Хранилище", "Завершение"}

// errNotSaving refuses the tasks that need Save Mode on a PC that is not in it.
var errNotSaving = errors.New("ПК не в режиме сохранения (Save Mode)")

// openSaveStore opens the store and checks that it holds a deploy into VHDX.
func (w *worker) openSaveStore() (savedStore, proto.StoreState, error) {
	if w.openStore == nil {
		return nil, proto.StoreState{}, errors.New("хранилище недоступно")
	}
	s, err := w.openStore()
	if err != nil {
		return nil, proto.StoreState{}, fmt.Errorf("хранилище: %w", err)
	}
	st := s.State()
	if st.Base == "" {
		s.Close()
		return nil, st, errors.New("на ПК нет деплоя в VHDX: сначала «Развернуть в VHDX»")
	}
	return s, st, nil
}

func (w *worker) saveScreen(title, detail string) {
	w.names = saveStages
	w.setStage(stageCheck)
	w.screen.update(func(st *ui.State) {
		st.Phase, st.Percent = ui.PhaseDeploy, -1
		st.Center, st.Caption = "", "хранилище"
		st.Title, st.Detail, st.Hint = title, detail, "Не выключайте компьютер"
	})
}

// runSaveMode puts the PC into Save Mode.
func (w *worker) runSaveMode(task *proto.AgentTask) error {
	w.saveScreen("Режим сохранения", "save.vhdx поверх base")
	s, st, err := w.openSaveStore()
	if err != nil {
		return err
	}
	defer s.Close()
	message := fmt.Sprintf("Save Mode: %s v%d, изменения копятся в %s", st.Image, st.Version, proto.SaveFile)
	if st.Save == "" {
		if err := s.EnterSave(); err != nil {
			return fmt.Errorf("режим сохранения не включён: %w", err)
		}
	} else {
		message = "ПК уже в режиме сохранения"
	}
	w.setStage(stageFinish)
	if err := w.finish(task.ID, true, message); err != nil {
		return err
	}
	w.screen.log(message)
	w.showResult("Режим сохранения", "Windows загрузится с "+proto.SaveFile+": изменения переживут перезагрузку")
	return nil
}

// runSaveDiscard leaves Save Mode and throws its changes away.
func (w *worker) runSaveDiscard(task *proto.AgentTask) error {
	w.saveScreen("Выход из режима сохранения", "изменения "+proto.SaveFile+" удаляются")
	s, st, err := w.openSaveStore()
	if err != nil {
		return err
	}
	defer s.Close()
	message := "ПК не был в режиме сохранения"
	if st.Save != "" {
		if err := s.LeaveSave(task.Accounts); err != nil {
			return fmt.Errorf("режим сохранения не выключен: %w", err)
		}
		message = fmt.Sprintf("режим сохранения выключен без сохранения, %s v%d", st.Image, st.Version)
	}
	w.setStage(stageFinish)
	if err := w.finish(task.ID, true, message); err != nil {
		return err
	}
	w.screen.log(message)
	w.showResult("Готово", "Компьютер загрузится как обычно, со сбросом")
	return nil
}

// runSaveImage clones the disk of Save Mode into a new version of the image.
func (w *worker) runSaveImage(task *proto.AgentTask) error {
	w.names = cloneStages
	w.setStage(stageCheck)
	w.screen.update(func(st *ui.State) {
		st.Phase, st.Percent = ui.PhaseCapture, 0
		st.Center, st.Caption = "0%", "подготовка"
		st.Title, st.Detail = "Сохранение в образ", "Образ "+task.Image+" · не выключайте компьютер"
	})
	w.rec = w.newRecorder()
	w.rec.Step("Открытие хранилища")
	s, st, err := w.openSaveStore()
	if err != nil {
		return err
	}
	defer s.Close()
	if st.Save == "" {
		return errNotSaving
	}
	if st.Image != task.Image {
		return fmt.Errorf("на ПК образ %s, а сохранение в %s: смените образ у ПК в панели", st.Image, task.Image)
	}
	src, err := s.OpenSaved()
	if err != nil {
		return fmt.Errorf("не удалось подключить %s: %w", proto.SaveFile, err)
	}
	defer src.Close()
	w.screen.log("диск: " + src.Describe())
	w.rec.Note("Диск", src.Describe())

	w.rec.Step("Ожидание очереди на образ")
	err = w.waitTurn(task, func() error { return w.client.cloneClaim(task.ID) })
	if errors.Is(err, errTaskGone) {
		return w.cancelled()
	}
	if err != nil {
		return fmt.Errorf("сервер не дал очередь на образ: %w", err)
	}

	// The volume is shrunk as in any clone, so the image fits the PCs with smaller
	// disks. The table stays here and not on the server: the disk is our own file,
	// and a later task must never apply it to the physical disk.
	w.rec.Step("Сжатие тома")
	table, err := src.Table()
	if err != nil {
		return fmt.Errorf("не удалось прочитать таблицу разделов: %w", err)
	}
	shrunk := src.Shrink(w.shrinkLog())
	if !shrunk {
		w.rec.Note("Сжатие тома", "не сжат"+w.shrinkWhy())
	}
	var m proto.Manifest
	err = recovery.Run(func() (err error) {
		m, err = w.capture(task, src)
		return err
	})
	if err != nil && !errors.Is(err, errTaskGone) {
		w.client.send(task.ID, "abort", "application/json", nil) // best effort: frees the server now
	}
	// Save Mode goes on if the saving failed, so the volume gets its size back.
	if shrunk {
		w.rec.Step("Возврат размера тома")
		if back := w.restoreSaved(src, table); back != nil {
			w.screen.log("том не восстановлен: " + back.Error())
		}
	}
	switch {
	case errors.Is(err, errTaskGone):
		return w.cancelled()
	case err != nil:
		return err
	}

	w.setStage(stageFinish)
	w.rec.Step("Проверка и сохранение на сервере")
	res, err := w.client.cloneFinish(task.ID, m)
	if errors.Is(err, errTaskGone) {
		return w.cancelled()
	}
	if err != nil {
		return fmt.Errorf("сервер не принял образ: %w", err)
	}
	message := fmt.Sprintf("%s, %s; дальше развёртывание этой версии на ПК", res.File, formatBytes(uint64(res.Size)))
	if err := w.finish(task.ID, true, message); err != nil {
		return err
	}
	w.screen.log("сервер: сохранён " + message)
	w.showResult("Готово", "Образ "+task.Image+" сохранён: "+res.File+". Дальше ПК развернёт эту версию")
	return nil
}

// restoreSaved gives the volume in save.vhdx its size back after the clone.
func (w *worker) restoreSaved(src source, table []byte) error {
	var err error
	for try := range restoreTries {
		if try > 0 {
			w.sleep(retryDelay)
		}
		if err = src.Repair(table); err == nil || errors.Is(err, errStaleTable) {
			return nil
		}
	}
	return err
}
