package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"locless-vhd/agent/ui"
	"locless-vhd/proto"
	"locless-vhd/recovery"
)

// confirmSeconds is how long the screen warns before the disk is touched, unless
// the panel says otherwise. It is the last chance to stop a deploy started on the
// wrong PC.
const confirmSeconds = 20

// confirmFor is the warning the task asks for.
func confirmFor(task *proto.AgentTask) int {
	if task == nil || task.ConfirmSeconds == nil || *task.ConfirmSeconds < 0 {
		return confirmSeconds
	}
	return min(*task.ConfirmSeconds, proto.MaxConfirmSeconds)
}

// imageWait is how often a waiting task asks whether its turn has come.
const imageWait = 10 * time.Second

var deployStages = []string{"Сеть", "Сервер", "Запись диска", "Завершение"}

var formatStages = []string{"Сеть", "Сервер", "Форматирование", "Завершение"}

var prepareStages = []string{"Сеть", "Сервер", "Подготовка диска", "Завершение"}

// target is a disk about to be overwritten.
type target interface {
	Describe() string
	// Fit builds the table of the image for this disk, or refuses a disk that cannot
	// hold it. It changes nothing.
	Fit(m proto.Manifest) (proto.Manifest, error)
	// Prepare takes the disk from Windows and writes the partition table. From
	// here on the old contents are lost.
	Prepare(m proto.Manifest) error
	// WriteBlock writes data at offset within a partition.
	WriteBlock(partition uint32, offset uint64, data []byte) error
	// Finish flushes everything to the disk.
	Finish() error
	Close() error
	// Grow stretches the file system over the partition. It runs after Close.
	Grow(m proto.Manifest) error
	// Format wipes the disk and leaves one empty NTFS volume with this label on all
	// of it, without Prepare. Nothing else may hold the disk.
	Format(label string) error
	// MakeStore wipes the disk and lays it out for Windows booted from a virtual
	// disk, see proto.StoreLayout: an empty EFI, MSR and the store formatted as
	// NTFS. Like Format it needs nothing else holding the disk.
	MakeStore() error
	// MakeBootable adds Windows to the boot menu of the firmware, which a disk
	// image cannot carry. It runs after Close, with the partitions back in Windows.
	MakeBootable(m proto.Manifest) error
	// SetName gives the Windows on the disk its computer name. It runs after
	// MakeBootable, which mounts that Windows.
	SetName(name string) error
}

// diskInfo is what is known about a disk before choosing one to overwrite.
type diskInfo struct {
	number   int
	internal bool // inside the PC: not a network disk, USB, a card or a VHD
	hasESP   bool // GPT with an EFI system partition: a disk Windows boots from
	empty    bool // no partitions at all
}

// chooseTarget picks the disk to overwrite. A disk Windows boots from comes first,
// by the same rule as a clone. Without one the PC has a new or a foreign disk, which
// is fine as long as there is no doubt which one: the only disk, or the only empty
// one among several. Otherwise a data disk could be wiped, so the deploy stops.
func chooseTarget(disks []diskInfo) (int, error) {
	var inside, empty []int
	for _, d := range disks {
		if !d.internal {
			continue
		}
		if d.hasESP {
			return d.number, nil
		}
		inside = append(inside, d.number)
		if d.empty {
			empty = append(empty, d.number)
		}
	}
	switch {
	case len(inside) == 0:
		return 0, errors.New("в компьютере не найден внутренний диск")
	case len(inside) == 1:
		return inside[0], nil
	case len(empty) == 1:
		return empty[0], nil
	}
	return 0, fmt.Errorf("внутренних дисков %d, и ни один не загрузочный: непонятно, какой стирать", len(inside))
}

// gptArea returns where partitions may lie on a disk that gets a new GPT: after the
// protective MBR, the header and 16 KiB of entries, and before their backup copy.
func gptArea(size uint64, sector uint32) (uint64, uint64) {
	entries := uint64(16384)
	first := 2*uint64(sector) + entries
	return first, size - entries - uint64(sector)
}

// vhdTarget is a target that writes the image into a virtual disk on the store.
type vhdTarget interface {
	target
	// VHDSize is the size of the virtual disk, chosen by the store if the image
	// leaves it open.
	VHDSize() uint64
	// Boot puts the differencing disk on top of the written base, makes Windows
	// boot from it and records state on the store. It runs after SetName. With
	// accounts the first boot sets them up and then makes the differencing disk.
	Boot(state proto.StoreState, accounts *proto.Accounts) error
}

// updater is a target that may already hold an earlier version of the image, laid
// out the same way. Then only the places whose data differs are written.
type updater interface {
	// Held returns what an earlier deploy of image wrote, if it finished.
	Held(image string) (proto.BaseIndex, bool)
	// Reopen readies the disk for writing over what it holds, with its table as it
	// is. It forgets what was held before the first write, so a deploy that stops
	// halfway is followed by a whole one.
	Reopen(m proto.Manifest) error
	// Remember records what the disk holds once a deploy has written all of it.
	Remember(proto.BaseIndex) error
}

// fitFresh plans the table of the image on a new virtual disk of size bytes.
func fitFresh(m proto.Manifest, size uint64) (proto.Manifest, error) {
	if m.SectorSize != 512 && m.SectorSize != 4096 {
		return proto.Manifest{}, fmt.Errorf("сектор образа %d байт, VHDX умеет 512 и 4096", m.SectorSize)
	}
	first, end := gptArea(size, m.SectorSize)
	fitted, err := m.Fit(first, end)
	var small *proto.TooSmallError
	if errors.As(err, &small) {
		return proto.Manifest{}, fmt.Errorf("образу нужно %s, а VHDX %s: увеличьте размер VHDX у образа",
			formatBytes(small.Need), formatBytes(size))
	}
	if err != nil {
		return proto.Manifest{}, err
	}
	fitted.DiskSize = size
	return fitted, nil
}

// runDeploy writes the image over the disk.
func (w *worker) runDeploy(task *proto.AgentTask) error {
	return w.deployInto(task, "Развёртывание образа", w.openTarget, nil)
}

// runDeployVHD writes the image into base.vhdx on the store of the PC.
func (w *worker) runDeployVHD(task *proto.AgentTask) error {
	var vt vhdTarget
	open := func() (target, error) {
		var err error
		vt, err = w.openVHD(task.VHDSize)
		return vt, err
	}
	boot := func(info proto.DeployInfo) error {
		return vt.Boot(proto.StoreState{Image: task.Image, Version: info.Version, BaseID: info.ID, VHDSize: vt.VHDSize(),
			Written: time.Now()}, task.Accounts)
	}
	return w.deployInto(task, "Развёртывание в VHDX", open, boot)
}

// deployInto writes the image into what open gives. boot, if set, runs last and
// its failure fails the deploy.
func (w *worker) deployInto(task *proto.AgentTask, title string, open func() (target, error), boot func(proto.DeployInfo) error) error {
	w.names = deployStages
	w.setStage(stageCheck)
	w.screen.update(func(st *ui.State) {
		st.Phase, st.Percent = ui.PhaseDeploy, 0
		st.Center, st.Caption = "0%", "подготовка"
		st.Title, st.Detail = title, "Образ "+task.Image
	})
	w.rec = w.newRecorder()
	w.rec.Step("Открытие диска")

	dst, err := open()
	if err != nil {
		return fmt.Errorf("не удалось открыть диск: %w", err)
	}
	defer dst.Close()
	w.screen.log("диск: " + dst.Describe())
	w.rec.Note("Диск", dst.Describe())

	w.rec.Step("Ожидание очереди на образ")
	var info proto.DeployInfo
	err = w.waitTurn(task, func() (err error) {
		info, err = w.client.deployStart(task.ID)
		return err
	})
	if errors.Is(err, errTaskGone) {
		return w.cancelled()
	}
	if err != nil {
		return fmt.Errorf("сервер не начал отдачу: %w", err)
	}
	// Best effort: however this ends, the server can close the image files now.
	defer w.client.post2(w.client.deployURL(task.ID, "abort"), "application/json", nil)
	fitted, err := dst.Fit(info.Manifest)
	if err != nil {
		return err
	}

	w.rec.Step("Отсчёт перед стиранием")
	if !w.confirmWipe(dst, "образ "+task.Image) {
		w.screen.log("отменено на компьютере")
		if err := w.finish(task.ID, false, "отменено на компьютере"); err != nil {
			return err
		}
		w.showResult("Отменено", "Диск не изменён, компьютер загрузится с него")
		return nil
	}

	w.screen.update(func(st *ui.State) { st.Hint = "Не выключайте компьютер" })
	d := deployRun{w: w, dst: dst, task: task.ID, info: info}
	held, update := heldUpdate(dst, task.Image, info, fitted)
	if update {
		w.rec.Step("Обновление поверх прежней версии")
		w.rec.Note("Обновление", fmt.Sprintf("v%d → v%d", held.Version, info.Version))
		w.screen.log(fmt.Sprintf("на диске v%d, пишутся только изменения", held.Version))
		if err := dst.(updater).Reopen(fitted); err != nil {
			return fmt.Errorf("не удалось открыть диск для обновления: %w", err)
		}
		d.held = make(map[[2]uint32]proto.Sum, len(held.Entries))
		for _, e := range held.Entries {
			d.held[[2]uint32{e.Partition, e.Position}] = e.Hash
		}
	} else {
		w.rec.Step("Таблица разделов")
		if err := dst.Prepare(fitted); err != nil {
			return fmt.Errorf("не удалось подготовить диск: %w", err)
		}
		w.screen.log("таблица разделов записана")
	}
	if len(task.Table) > 0 {
		w.forgetTable() // the volume it was saved for is gone
	}

	d.started = time.Now()
	d.speeds.update(d.started, "", 0, w.client.traffic) // the start, for the first speed
	switch err := d.write(); {
	case errors.Is(err, errTaskGone):
		// The disk is already overwritten in part: it must not look finished.
		w.screen.log("сервер: задача отменена, диск записан не полностью")
		w.showResult("Задача отменена", "Диск записан не полностью, Windows может не загрузиться")
		return nil
	case err != nil:
		return err
	}

	w.setStage(stageFinish)
	w.rec.Step("Сброс записи на диск")
	if err := dst.Finish(); err != nil {
		return fmt.Errorf("не удалось завершить запись: %w", err)
	}
	if err := dst.Close(); err != nil {
		return fmt.Errorf("не удалось завершить запись: %w", err)
	}
	writing := time.Since(d.started)
	// Without the record the next deploy writes everything, which is slower and
	// nothing worse, so a failure only goes to the log.
	if u, ok := dst.(updater); ok {
		x := proto.BaseIndex{Image: task.Image, Version: info.Version, BlockSize: info.BlockSize, Manifest: fitted, Entries: d.entries}
		if err := u.Remember(x); err != nil {
			w.screen.log("список блоков base не сохранён: " + err.Error())
		}
	}
	w.rec.Step("Расширение тома")
	if err := dst.Grow(fitted); err != nil {
		// Windows still boots, on a smaller volume.
		w.screen.log("том не расширен: " + err.Error())
	}
	// The image is on the disk either way: without this entry the PC still boots
	// through the network or the fallback loader, so a failure only goes to the log.
	w.rec.Step("Загрузочная запись")
	if err := dst.MakeBootable(fitted); err != nil {
		w.screen.log("загрузочная запись: " + err.Error())
	}
	w.rec.Step("Имя ПК")
	named := w.nameWindows(dst)
	if boot != nil {
		w.rec.Step("Разностный диск и загрузка")
		if err := boot(info); err != nil {
			return fmt.Errorf("образ записан, но загрузка с VHDX не настроена: %w", err)
		}
	}
	w.rec.Step("Отчёт серверу")
	message := fmt.Sprintf("%s v%d, записано %s за %s, %s", task.Image, info.Version, formatBytes(d.written),
		formatDuration(writing), named)
	if update {
		message = fmt.Sprintf("%s v%d → v%d: изменилось %d блоков из %d, записано %s за %s, %s", task.Image,
			held.Version, info.Version, len(d.entries)-d.unchanged, len(d.entries), formatBytes(d.written),
			formatDuration(writing), named)
	}
	if err := w.finish(task.ID, true, message); err != nil {
		return err
	}
	w.screen.log("готово: " + message)
	w.showResult("Готово", "Образ "+task.Image+" развёрнут")
	return nil
}

// heldUpdate says whether the deploy may write only what differs from what the
// disk holds: the same image, a finished earlier deploy, the same block size and
// the same table on this disk. Anything else is a whole deploy.
func heldUpdate(dst target, image string, info proto.DeployInfo, fitted proto.Manifest) (proto.BaseIndex, bool) {
	u, ok := dst.(updater)
	if !ok {
		return proto.BaseIndex{}, false
	}
	held, ok := u.Held(image)
	if !ok || held.Image != image || held.BlockSize != info.BlockSize || !proto.SameLayout(held.Manifest, fitted) {
		return proto.BaseIndex{}, false
	}
	return held, true
}

// nameWindows gives the deployed Windows the name the PC has in the panel and says
// how that went. Windows boots under the name of the image without it, so a failure
// does not fail the deploy, but the panel has to show it.
func (w *worker) nameWindows(dst target) string {
	err := checkComputerName(w.cfg.Host)
	if err == nil {
		err = dst.SetName(w.cfg.Host)
	}
	if err != nil {
		return "имя ПК не задано: " + err.Error()
	}
	return "имя ПК " + w.cfg.Host
}

// maxComputerName is the limit of a NetBIOS name, which Windows still enforces.
const maxComputerName = 15

// checkComputerName refuses what Windows would not accept as a computer name. The
// panel allows any name, and changing it here would hide that from the operator.
func checkComputerName(name string) error {
	if name == "" || len(name) > maxComputerName {
		return fmt.Errorf("%q: нужно от 1 до %d символов", name, maxComputerName)
	}
	digits := true
	for _, c := range []byte(name) {
		isDigit := c >= '0' && c <= '9'
		digits = digits && isDigit
		if !isDigit && c != '-' && (c < 'a' || c > 'z') && (c < 'A' || c > 'Z') {
			return fmt.Errorf("%q: только латиница, цифры и дефис", name)
		}
	}
	if digits || name[0] == '-' || name[len(name)-1] == '-' {
		return fmt.Errorf("%q: не может быть из одних цифр, начинаться или кончаться дефисом", name)
	}
	return nil
}

// controlSet finds the control set Windows boots with in what reg query prints for
// the value Current of the key Select.
func controlSet(query string) (string, error) {
	fields := strings.Fields(query)
	for i, f := range fields[:max(len(fields)-1, 0)] {
		if f == "REG_DWORD" {
			n, err := strconv.ParseUint(fields[i+1], 0, 32)
			if err != nil {
				break
			}
			return fmt.Sprintf("ControlSet%03d", n), nil
		}
	}
	return "", errors.New("в реестре не найден текущий ControlSet")
}

// wipeJob is a task that wipes the disk and lays it out anew, with no image.
type wipeJob struct {
	stages  []string
	title   string // on the screen while the task runs
	result  string // what the disk becomes, for the countdown and the report
	caption string // under the spinner while the disk is changed
	failure string // starts the error
	done    string // the last screen
	run     func(target) error
}

var formatJob = wipeJob{
	stages: formatStages,
	title:  "Форматирование диска", result: "том " + proto.FormatLabel, caption: "форматирование",
	failure: "не удалось отформатировать диск", done: "Диск отформатирован: том " + proto.FormatLabel,
	run: func(dst target) error { return dst.Format(proto.FormatLabel) },
}

var prepareJob = wipeJob{
	stages: prepareStages,
	title:  "Подготовка диска", result: "EFI, MSR и хранилище " + proto.StoreLabel, caption: "разметка",
	failure: "не удалось подготовить диск", done: "Диск подготовлен: хранилище " + proto.StoreLabel,
	run: func(dst target) error { return dst.MakeStore() },
}

// runWipe runs a wipeJob after the countdown that lets the PC be saved.
func (w *worker) runWipe(task *proto.AgentTask, job wipeJob) error {
	w.names = job.stages
	w.setStage(stageCheck)
	w.screen.update(func(st *ui.State) {
		st.Phase, st.Percent = ui.PhaseDeploy, 0
		st.Center, st.Caption = "0%", "подготовка"
		st.Title, st.Detail = job.title, job.result
	})
	w.rec = w.newRecorder()
	w.rec.Step("Открытие диска")
	dst, err := w.openTarget()
	if err != nil {
		return fmt.Errorf("не удалось открыть диск: %w", err)
	}
	defer dst.Close()
	w.screen.log("диск: " + dst.Describe())

	w.rec.Step("Отсчёт перед стиранием")
	if !w.confirmWipe(dst, job.result) {
		w.screen.log("отменено на компьютере")
		if err := w.finish(task.ID, false, "отменено на компьютере"); err != nil {
			return err
		}
		w.showResult("Отменено", "Диск не изменён")
		return nil
	}
	w.screen.update(func(st *ui.State) {
		st.Center, st.Caption, st.Hint = "…", job.caption, "Не выключайте компьютер"
	})
	w.rec.Step("Разметка и форматирование")
	if err := job.run(dst); err != nil {
		return fmt.Errorf("%s: %w", job.failure, err)
	}
	if len(task.Table) > 0 {
		w.forgetTable() // the volume it was saved for is gone
	}
	w.setStage(stageFinish)
	w.rec.Step("Отчёт серверу")
	message := dst.Describe() + ", " + job.result
	if err := w.finish(task.ID, true, message); err != nil {
		return err
	}
	w.screen.log("готово: " + message)
	w.showResult("Готово", job.done)
	return nil
}

// waitTurn repeats ask while another PC has the image. The disk is still as it was,
// so waiting costs nothing.
func (w *worker) waitTurn(task *proto.AgentTask, ask func() error) error {
	var before ui.State
	waiting := false
	for {
		err := ask()
		if !errors.Is(err, errImageBusy) {
			if waiting {
				w.screen.log("очередь подошла")
				w.screen.update(func(st *ui.State) {
					st.Title, st.Detail, st.Center, st.Caption = before.Title, before.Detail, before.Center, before.Caption
				})
			}
			return err
		}
		if !waiting {
			waiting = true
			before = w.screen.snapshot()
			w.screen.log("образ занят другим ПК, ожидание")
			w.screen.update(func(st *ui.State) {
				st.Title, st.Detail = "Ожидание очереди", "Образ "+task.Image+" занят другим компьютером, диск не изменён"
				st.Center, st.Caption = "…", "ожидание"
			})
		}
		// The report is also what tells a waiting agent that the task was cancelled.
		err = w.client.progress(task.ID, 0, proto.StageWaiting, "ждёт очереди на образ "+task.Image)
		if errors.Is(err, errTaskGone) {
			return err
		}
		if err != nil {
			w.screen.log("отчёт не доставлен: " + err.Error())
		}
		w.sleep(imageWait)
	}
}

// confirmWipe warns on the screen and returns false if the operator pressed Esc.
// With no warning set it still honours an Esc pressed before.
func (w *worker) confirmWipe(dst target, result string) bool {
	if w.confirm == 0 {
		select {
		case <-w.abort:
			return false
		default:
			return true
		}
	}
	before := w.screen.snapshot()
	w.screen.update(func(st *ui.State) {
		st.Title = "Диск будет стёрт"
		st.Detail = dst.Describe() + " → " + result
	})
	for left := w.confirm; left > 0; left-- {
		w.screen.update(func(st *ui.State) {
			st.Center, st.Caption = fmt.Sprint(left), "до начала записи"
			st.Hint = "Esc — отменить"
		})
		select {
		case <-w.abort:
			return false
		default:
		}
		w.sleep(time.Second)
	}
	w.screen.update(func(st *ui.State) { st.Title, st.Detail = before.Title, before.Detail })
	return true
}

// deployRun is the state of one pass over the image.
type deployRun struct {
	w         *worker
	dst       target
	task      int64
	info      proto.DeployInfo
	started   time.Time
	reported  time.Time
	entries   []proto.DeployEntry
	held      map[[2]uint32]proto.Sum // what the disk holds already, nil for a whole deploy
	unchanged int                     // places skipped because the disk holds them already
	done      int
	written   uint64
	sizes     map[uint32]uint64
	speeds    speeds
}

// write fills every place of the list on the disk.
func (d *deployRun) write() error {
	rec := d.w.rec
	rec.Step("Список блоков")
	var err error
	if d.entries, err = d.w.client.deployList(d.task); err != nil {
		return err
	}
	d.sizes = map[uint32]uint64{}
	for _, p := range d.info.Manifest.Partitions {
		if p.Content == proto.ContentBlocks {
			d.sizes[p.Number] = p.Size
		}
	}

	// Every place of the list is a different one, so the order of writing is free.
	var batches [][]uint32
	var batch []uint32
	zeros := make([]byte, d.info.BlockSize)
	rec.Step("Запись нулевых блоков")
	for i, e := range d.entries {
		if d.held != nil {
			if hash, ok := d.held[[2]uint32{e.Partition, e.Position}]; ok && hash == e.Hash {
				d.unchanged++
				d.done++
				rec.Count("блоков без изменений", 1)
				continue
			}
		}
		if e.Zero() {
			if err := d.writeZero(e, zeros); err != nil {
				return err
			}
			rec.Count("нулевых блоков", 1)
			continue
		}
		rec.Count("блоков с данными", 1)
		batch = append(batch, uint32(i))
		if len(batch) == cloneBatch {
			batches = append(batches, batch)
			batch = nil
		}
	}
	if len(batch) > 0 {
		batches = append(batches, batch)
	}
	rec.Step("Загрузка и запись блоков")
	return d.pipeline(batches)
}

// Kinds of work of a deploy, as the metrics name them.
const (
	workFetch       = "загрузка с сервера"
	workDecode      = "распаковка и проверка"
	workWrite       = "запись на диск"
	workUnpackWaits = "распаковка ждёт запись"
	workFetchWaits  = "загрузка ждёт распаковку"
	workWriteWaits  = "запись ждёт загрузку"
)

// pipeline keeps the network, the processor and the disk busy at once: one batch is
// on its way from the server while the one before it is unpacked and the one before
// that is written. Only this goroutine writes. Nothing runs on after it returns.
func (d *deployRun) pipeline(batches [][]uint32) error {
	ctx, cancel := context.WithCancel(context.Background())
	var wg sync.WaitGroup
	defer wg.Wait()
	defer cancel()

	// Batches wait here still compressed, about half the size of raw ones: a reserve
	// for the moments the server is slow, as when its disk is busy with something
	// else. Measured on PC08, the disk of the PC stood idle 56 s of 271 for want of it.
	fetched := make(chan deployBatch, fetchedReserve)
	unpacked := make(chan deployBatch, 1)
	free := newSlabs(d.info.BlockSize)
	wg.Go(func() { d.fetch(ctx, batches, fetched) })
	wg.Go(func() { d.unpack(ctx, fetched, unpacked, free) })

	rec := d.w.rec
	for {
		waited := rec.Time(workWriteWaits)
		b, ok := <-unpacked
		waited()
		if !ok {
			return nil
		}
		if b.err != nil {
			return b.err
		}
		written := rec.Time(workWrite)
		err := d.store(b)
		written()
		if err != nil {
			return err
		}
		free <- b.slab
	}
}

// deployBatch is one batch on its way through the pipeline. A stage that fails puts
// its error here and stops, so the error arrives behind the batches before it.
type deployBatch struct {
	numbers []uint32
	body    []byte   // the stored blocks, as the server sent them
	slab    []byte   // where the blocks are unpacked to
	raw     [][]byte // the blocks, checked and ready to write
	err     error
}

// fetchRequests is how many batches the agent asks for at once. With one, the
// server read a batch, sent it and stood idle until the next request came: its
// disk worked in bursts. Two keep it reading while the agent receives the other.
const fetchRequests = 4

// fetchedReserve is how many downloaded batches may wait for unpacking.
const fetchedReserve = 4

// writeParallel is how many blocks of a batch are written at once. A SATA disk
// gains little from it, an NVMe one needs it to reach its speed.
const writeParallel = 4

// fetch downloads the batches, fetchRequests at a time. They arrive in any order:
// every batch fills its own places on the disk.
func (d *deployRun) fetch(ctx context.Context, batches [][]uint32, out chan<- deployBatch) {
	defer close(out)
	todo := make(chan []uint32)
	failed := make(chan struct{})
	var once sync.Once
	var wg sync.WaitGroup
	for range fetchRequests {
		wg.Go(func() {
			for numbers := range todo {
				b := deployBatch{numbers: numbers}
				fetched := d.w.rec.Time(workFetch)
				b.err = recovery.Run(func() (err error) { // a panic here would end the whole agent
					b.body, err = d.w.client.deployBlocks(ctx, d.task, numbers)
					return err
				})
				fetched()
				d.w.rec.Bytes("загружено по сети", uint64(len(b.body)))
				d.w.rec.Count("пачек", 1)
				waited := d.w.rec.Time(workFetchWaits)
				select {
				case out <- b:
					waited()
				case <-ctx.Done():
					return
				}
				if b.err != nil {
					once.Do(func() { close(failed) })
					return
				}
			}
		})
	}
	defer wg.Wait()
	defer close(todo)
	for _, numbers := range batches {
		select {
		case todo <- numbers:
		case <-failed:
			return
		case <-ctx.Done():
			return
		}
	}
}

// unpack checks and unpacks each batch. It waits for a free slab, which is what
// holds the network back when the disk is the slowest.
func (d *deployRun) unpack(ctx context.Context, in <-chan deployBatch, out chan<- deployBatch, free <-chan []byte) {
	defer close(out)
	for b := range in {
		if b.err == nil {
			waited := d.w.rec.Time(workUnpackWaits)
			select {
			case b.slab = <-free:
				waited()
			case <-ctx.Done():
				return
			}
			decoded := d.w.rec.Time(workDecode)
			b.err = recovery.Run(func() error { return d.decode(&b) })
			decoded()
		}
		waited := d.w.rec.Time(workUnpackWaits)
		select {
		case out <- b:
			waited()
		case <-ctx.Done():
			return
		}
		if b.err != nil {
			return
		}
	}
}

// decode unpacks the blocks of a batch in parallel and checks each against the list.
func (d *deployRun) decode(b *deployBatch) error {
	b.raw = make([][]byte, len(b.numbers))
	errs := make([]error, len(b.numbers))
	slots := make(chan struct{}, runtime.NumCPU())
	size := int(d.info.BlockSize)
	var wg sync.WaitGroup
	defer wg.Wait()
	r := bytes.NewReader(b.body)
	for i, n := range b.numbers {
		e := d.entries[n]
		h, payload, err := proto.ReadStored(r)
		if err != nil {
			return fmt.Errorf("сервер прислал неполные данные: %w", err)
		}
		slots <- struct{}{}
		wg.Go(func() {
			defer func() { <-slots }()
			errs[i] = recovery.Run(func() error { // a panic here would end the whole agent
				raw, err := proto.DecodeBlockInto(b.slab[i*size:i*size:(i+1)*size], h, payload)
				if err != nil {
					return fmt.Errorf("блок %d раздела %d повреждён: %w", e.Position, e.Partition, err)
				}
				if h.Hash != e.Hash {
					return fmt.Errorf("сервер прислал другой блок вместо %d раздела %d", e.Position, e.Partition)
				}
				b.raw[i] = raw
				return nil
			})
		})
	}
	wg.Wait()
	return errors.Join(errs...)
}

// place returns where an entry goes and how long the block must be.
func (d *deployRun) place(e proto.DeployEntry) (uint64, uint64, error) {
	size, ok := d.sizes[e.Partition]
	offset := uint64(e.Position) * uint64(d.info.BlockSize)
	if !ok || offset >= size {
		return 0, 0, fmt.Errorf("блок %d раздела %d вне образа", e.Position, e.Partition)
	}
	return offset, min(uint64(d.info.BlockSize), size-offset), nil
}

func (d *deployRun) writeZero(e proto.DeployEntry, zeros []byte) error {
	offset, length, err := d.place(e)
	if err != nil {
		return err
	}
	if err := d.dst.WriteBlock(e.Partition, offset, zeros[:length]); err != nil {
		return fmt.Errorf("ошибка записи раздела %d: %w", e.Partition, err)
	}
	d.written += length
	d.w.rec.Bytes("записано на диск", length)
	d.done++
	return nil
}

// store writes the checked blocks of a batch to the disk, writeParallel at a
// time: every block has its own place, so the order does not matter.
func (d *deployRun) store(b deployBatch) error {
	type place struct {
		partition uint32
		offset    uint64
		raw       []byte
	}
	places := make([]place, len(b.numbers))
	for i, n := range b.numbers {
		e := d.entries[n]
		offset, length, err := d.place(e)
		if err != nil {
			return err
		}
		if uint64(len(b.raw[i])) != length {
			return fmt.Errorf("блок %d раздела %d: %d байт вместо %d", e.Position, e.Partition, len(b.raw[i]), length)
		}
		places[i] = place{e.Partition, offset, b.raw[i]}
	}
	next := make(chan place)
	errs := make(chan error, writeParallel)
	var failed atomic.Bool // after an error only the writes already started finish
	var wg sync.WaitGroup
	for range writeParallel {
		wg.Go(func() {
			for p := range next {
				if failed.Load() {
					continue // let the sender finish
				}
				if err := d.dst.WriteBlock(p.partition, p.offset, p.raw); err != nil {
					failed.Store(true)
					errs <- fmt.Errorf("ошибка записи раздела %d: %w", p.partition, err)
				}
			}
		})
	}
	for _, p := range places {
		next <- p
	}
	close(next)
	wg.Wait()
	close(errs)
	if err := <-errs; err != nil {
		return err
	}
	for _, p := range places {
		d.written += uint64(len(p.raw))
		d.w.rec.Bytes("записано на диск", uint64(len(p.raw)))
		d.done++
	}
	return d.report()
}

// report updates the screen and, now and then, the panel.
func (d *deployRun) report() error {
	d.w.rec.Sample()
	percent := min(d.done*100/max(len(d.entries), 1), 99)
	rates, summary := d.speeds.update(time.Now(), "Запись на диск", d.written, d.w.client.traffic)
	caption := "записано " + formatBytes(d.written)
	d.w.screen.update(func(st *ui.State) {
		st.Percent, st.Center, st.Caption = float64(percent), fmt.Sprintf("%d%%", percent), caption
		st.Rates = rates
	})
	if time.Since(d.reported) < reportEvery {
		return nil
	}
	d.reported = time.Now()
	err := d.w.client.progress(d.task, percent, deployStages[stageCheck], caption+" · "+summary)
	if errors.Is(err, errTaskGone) {
		return err
	}
	if err != nil {
		d.w.screen.log("отчёт не доставлен: " + err.Error())
	}
	return nil
}
