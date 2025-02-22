// Package commands provides the set of CLI commands used to communicate with the AIS cluster.
// This file handles cluster and daemon operations.
/*
 * Copyright (c) 2018-2022, NVIDIA CORPORATION. All rights reserved.
 */
package commands

import (
	"context"
	"fmt"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/NVIDIA/aistore/api"
	"github.com/NVIDIA/aistore/api/apc"
	"github.com/NVIDIA/aistore/cluster"
	"github.com/NVIDIA/aistore/cmd/cli/templates"
	"github.com/NVIDIA/aistore/cmn"
	"github.com/NVIDIA/aistore/cmn/cos"
	"github.com/NVIDIA/aistore/ios"
	"github.com/NVIDIA/aistore/stats"
	"github.com/NVIDIA/aistore/xact"
	"github.com/urfave/cli"
	"golang.org/x/sync/errgroup"
)

type (
	targetDiskStats struct {
		targetID string
		stats    ios.AllDiskStats
	}

	targetRebSnap struct {
		tid  string
		snap *xact.SnapExt
	}
)

var (
	pmapStatus = make(stats.DaemonStatusMap, 8)
	tmapStatus = make(stats.DaemonStatusMap, 8)
)

// Gets Smap from a given node (`daemonID`) and displays it
func clusterSmap(c *cli.Context, primarySmap *cluster.Smap, daemonID string, useJSON bool) error {
	var (
		smap = primarySmap
		err  error
	)
	if daemonID != "" {
		smap, err = api.GetNodeClusterMap(defaultAPIParams, daemonID)
		if err != nil {
			return err
		}
	}
	extendedURLs := false
	for _, m := range []cluster.NodeMap{smap.Tmap, smap.Pmap} {
		for _, v := range m {
			if v.PubNet != v.ControlNet || v.PubNet != v.DataNet {
				extendedURLs = true
			}
		}
	}
	body := templates.SmapTemplateHelper{
		Smap:         smap,
		ExtendedURLs: extendedURLs,
	}
	return templates.DisplayOutput(body, c.App.Writer, templates.SmapTmpl, useJSON)
}

func getBMD(c *cli.Context) error {
	useJSON := flagIsSet(c, jsonFlag)
	bmd, err := api.GetBMD(defaultAPIParams)
	if err != nil {
		return err
	}
	if useJSON {
		return templates.DisplayOutput(bmd, c.App.Writer, "", useJSON)
	}

	tw := &tabwriter.Writer{}
	tw.Init(c.App.Writer, 0, 8, 2, ' ', 0)
	if !flagIsSet(c, noHeaderFlag) {
		fmt.Fprintln(tw, "PROVIDER\tNAMESPACE\tNAME\tBACKEND\tCOPIES\tEC(D/P, minsize)\tCREATED")
	}
	for provider, namespaces := range bmd.Providers {
		for nsUname, buckets := range namespaces {
			ns := cmn.ParseNsUname(nsUname)
			for bucket, props := range buckets {
				var copies, ec string
				if props.Mirror.Enabled {
					copies = strconv.Itoa(int(props.Mirror.Copies))
				}
				if props.EC.Enabled {
					ec = fmt.Sprintf("%d/%d, %s", props.EC.DataSlices,
						props.EC.ParitySlices, cos.B2S(props.EC.ObjSizeLimit, 0))
				}
				fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\t%s\n",
					provider, ns, bucket, props.BackendBck, copies, ec,
					cos.FormatUnixNano(props.Created, ""))
			}
		}
	}
	tw.Flush()
	fmt.Fprintln(c.App.Writer)
	fmt.Fprintf(c.App.Writer, "Version:\t%d\n", bmd.Version)
	fmt.Fprintf(c.App.Writer, "UUID:\t\t%s\n", bmd.UUID)
	return nil
}

// Displays the status of the cluster or node
func clusterDaemonStatus(c *cli.Context, smap *cluster.Smap, cluConfig *cmn.ClusterConfig, daemonID string, useJSON, hideHeader bool) error {
	body := templates.StatusTemplateHelper{
		Smap:      smap,
		CluConfig: cluConfig,
		Status: templates.DaemonStatusTemplateHelper{
			Pmap: pmapStatus,
			Tmap: tmapStatus,
		},
	}
	if res, proxyOK := pmapStatus[daemonID]; proxyOK {
		return templates.DisplayOutput(res, c.App.Writer, templates.NewProxyTable(res, smap).Template(hideHeader), useJSON)
	} else if res, targetOK := tmapStatus[daemonID]; targetOK {
		return templates.DisplayOutput(res, c.App.Writer, templates.NewTargetTable(res).Template(hideHeader), useJSON)
	} else if daemonID == apc.Proxy {
		template := templates.NewProxiesTable(&body.Status, smap).Template(hideHeader)
		return templates.DisplayOutput(body, c.App.Writer, template, useJSON)
	} else if daemonID == apc.Target {
		return templates.DisplayOutput(body, c.App.Writer,
			templates.NewTargetsTable(&body.Status).Template(hideHeader), useJSON)
	} else if daemonID == "" {
		template := templates.NewProxiesTable(&body.Status, smap).Template(false) + "\n" +
			templates.NewTargetsTable(&body.Status).Template(false) + "\n" +
			templates.ClusterSummary
		return templates.DisplayOutput(body, c.App.Writer, template, useJSON)
	}
	return fmt.Errorf("%s is not a valid DAEMON_ID nor DAEMON_TYPE", daemonID)
}

// Displays the disk stats of a target
func daemonDiskStats(c *cli.Context, daemonID string, useJSON, hideHeader bool) error {
	if _, ok := pmapStatus[daemonID]; ok {
		return fmt.Errorf("daemon ID=%q is a proxy, but \"%s %s %s\" works only for targets",
			daemonID, cliName, commandShow, subcmdShowDisk)
	}
	if _, ok := tmapStatus[daemonID]; daemonID != "" && !ok {
		return fmt.Errorf("target ID=%q does not exist", daemonID)
	}

	targets := stats.DaemonStatusMap{daemonID: {}}
	if daemonID == "" {
		targets = tmapStatus
	}

	diskStats, err := getDiskStats(targets)
	if err != nil {
		return err
	}

	template := chooseTmpl(templates.DiskStatBodyTmpl, templates.DiskStatsFullTmpl, hideHeader)
	err = templates.DisplayOutput(diskStats, c.App.Writer, template, useJSON)
	if err != nil {
		return err
	}

	return nil
}

func getDiskStats(targets stats.DaemonStatusMap) ([]templates.DiskStatsTemplateHelper, error) {
	var (
		allStats = make([]templates.DiskStatsTemplateHelper, 0, len(targets))
		wg, _    = errgroup.WithContext(context.Background())
		statsCh  = make(chan targetDiskStats, len(targets))
	)

	for targetID := range targets {
		wg.Go(func(targetID string) func() error {
			return func() (err error) {
				diskStats, err := api.GetTargetDiskStats(defaultAPIParams, targetID)
				if err != nil {
					return err
				}

				statsCh <- targetDiskStats{stats: diskStats, targetID: targetID}
				return nil
			}
		}(targetID))
	}

	err := wg.Wait()
	close(statsCh)
	if err != nil {
		return nil, err
	}
	for diskStats := range statsCh {
		targetID := diskStats.targetID
		for diskName, diskStat := range diskStats.stats {
			allStats = append(allStats,
				templates.DiskStatsTemplateHelper{TargetID: targetID, DiskName: diskName, Stat: diskStat})
		}
	}

	sort.Slice(allStats, func(i, j int) bool {
		if allStats[i].TargetID != allStats[j].TargetID {
			return allStats[i].TargetID < allStats[j].TargetID
		}
		if allStats[i].DiskName != allStats[j].DiskName {
			return allStats[i].DiskName < allStats[j].DiskName
		}
		return allStats[i].Stat.Util > allStats[j].Stat.Util
	})

	return allStats, nil
}

func showRebalance(c *cli.Context, keepMonitoring bool, refreshRate time.Duration) error {
	var (
		tw                            = &tabwriter.Writer{}
		latestAborted, latestFinished bool
	)
	tw.Init(c.App.Writer, 0, 8, 2, ' ', 0)

	// run until rebalance is completed
	xactArgs := api.XactReqArgs{Kind: apc.ActRebalance}
	for {
		rebSnaps, err := api.QueryXactionSnaps(defaultAPIParams, xactArgs)
		if err != nil {
			switch err := err.(type) {
			case *cmn.ErrHTTP:
				if err.Status == http.StatusNotFound {
					fmt.Fprintln(c.App.Writer, "Rebalance has not started yet.")
					return nil
				}
				return err
			default:
				return err
			}
		}

		allSnaps := make([]*targetRebSnap, 0, 100)
		for daemonID, daemonStats := range rebSnaps {
			for _, sts := range daemonStats {
				allSnaps = append(allSnaps, &targetRebSnap{
					tid:  daemonID,
					snap: sts,
				})
			}
		}
		sort.Slice(allSnaps, func(i, j int) bool {
			if allSnaps[i].snap.ID != allSnaps[j].snap.ID {
				return allSnaps[i].snap.ID > allSnaps[j].snap.ID
			}
			return allSnaps[i].tid < allSnaps[j].tid
		})

		// NOTE: If changing header do not forget to change `colCount` couple
		//  lines below and `displayRebStats` logic.
		fmt.Fprintln(tw, "REB ID\t NODE\t OBJECTS RECV\t SIZE RECV\t OBJECTS SENT\t SIZE SENT\t START TIME\t END TIME\t ABORTED")
		prevID := ""
		for _, sts := range allSnaps {
			if flagIsSet(c, allXactionsFlag) {
				if prevID != "" && sts.snap.ID != prevID {
					fmt.Fprintln(tw, strings.Repeat("\t ", 9 /*colCount*/))
				}
				displayRebStats(tw, sts)
			} else {
				if prevID != "" && sts.snap.ID != prevID {
					break
				}
				latestAborted = latestAborted || sts.snap.AbortedX
				latestFinished = latestFinished || !sts.snap.EndTime.IsZero()
				displayRebStats(tw, sts)
			}
			prevID = sts.snap.ID
		}
		tw.Flush()

		if !flagIsSet(c, allXactionsFlag) {
			if latestFinished && latestAborted {
				fmt.Fprintln(c.App.Writer, "\nRebalance aborted.")
				break
			} else if latestFinished {
				fmt.Fprintln(c.App.Writer, "\nRebalance completed.")
				break
			}
		}

		if !keepMonitoring {
			break
		}

		time.Sleep(refreshRate)
	}

	return nil
}

func displayRebStats(tw *tabwriter.Writer, st *targetRebSnap) {
	endTime := templates.NotSetVal
	if !st.snap.EndTime.IsZero() {
		endTime = st.snap.EndTime.Format("01-02 15:04:05")
	}
	startTime := st.snap.StartTime.Format("01-02 15:04:05")

	fmt.Fprintf(tw,
		"%s\t %s\t %d\t %s\t %d\t %s\t %s\t %s\t %t\n",
		st.snap.ID, st.tid,
		st.snap.Snap.Stats.InObjs, cos.B2S(st.snap.Snap.Stats.InBytes, 2),
		st.snap.Snap.Stats.OutObjs, cos.B2S(st.snap.Snap.Stats.OutBytes, 2),
		startTime, endTime, st.snap.IsAborted(),
	)
}
